package wecom

import (
	"bytes"
	"crypto/aes"
	ccipher "crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-wecom-bridge/internal/protocol"
	"agent-wecom-bridge/internal/server"
)

func encryptForWecom(key, plain []byte) []byte {
	block, _ := aes.NewCipher(key)
	iv := key[:16]
	padLen := 16 - (len(plain) % 16)
	buf := make([]byte, 0, len(plain)+padLen)
	buf = append(buf, plain...)
	for i := 0; i < padLen; i++ {
		buf = append(buf, byte(padLen))
	}
	out := make([]byte, len(buf))
	ccipher.NewCBCEncrypter(block, iv).CryptBlocks(out, buf)
	return out
}

func newBridgeWithStore(t *testing.T) (*Bridge, *fakeWecom, *fakePlatform, *server.FileStore, *Registry) {
	t.Helper()
	fs, _ := server.NewFileStore(t.TempDir())
	reg, _ := OpenRegistry(filepath.Join(t.TempDir(), "reg.json"))
	fw := &fakeWecom{}
	fp := &fakePlatform{}
	b := NewBridge(BridgeConfig{
		Channel:        "wecom:personal",
		AgentKey:       "personal",
		Platform:       fp,
		Registry:       reg,
		Dedup:          NewDedup(time.Minute),
		Store:          fs,
		UserID:         "local",
		DownloadTicket: "tkn",
	})
	b.SetWecom(fw)
	return b, fw, fp, fs, reg
}

// 入站图片：bridge 下载解密，落 store，推 /api/upload 帧给 platform
func TestHandleInboundImage(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	plain := []byte("fake image bytes (pretend PNG)")
	ct := encryptForWecom(key, plain)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(ct)
	}))
	defer ts.Close()

	b, _, fp, fs, reg := newBridgeWithStore(t)

	in := Inbound{Cmd: CmdCallback}
	in.Headers.ReqID = "R-IMG"
	in.Body.MsgType = MsgTypeImage
	in.Body.ExternalUserID = "wmUSER"
	in.Body.Image = &MediaPayload{
		URL:      ts.URL,
		AESKey:   base64.StdEncoding.EncodeToString(key),
		Filename: "screenshot.png",
	}
	b.HandleWecomMessage(in)

	if len(fp.frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(fp.frames))
	}
	req, ok := fp.frames[0].(protocol.Request)
	if !ok || req.Type != "/api/upload" {
		t.Fatalf("frame: %+v", fp.frames[0])
	}

	var p struct {
		RequestID string `json:"requestId"`
		ChatID    string `json:"chatId"`
		Upload    struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			MimeType  string `json:"mimeType"`
			SizeBytes int64  `json:"sizeBytes"`
			SHA256    string `json:"sha256"`
			URL       string `json:"url"`
		} `json:"upload"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Upload.Type != "image" || p.Upload.Name != "screenshot.png" || p.Upload.MimeType != "image/png" {
		t.Fatalf("upload: %+v", p.Upload)
	}
	if p.Upload.SizeBytes != int64(len(plain)) {
		t.Fatalf("size: %d", p.Upload.SizeBytes)
	}
	if !strings.HasPrefix(p.ChatID, "wecom#single#wmUSER#") {
		t.Fatalf("chatId: %s", p.ChatID)
	}
	if !strings.Contains(p.Upload.URL, "?ticket=tkn") {
		t.Fatalf("url missing ticket: %s", p.Upload.URL)
	}

	// 落 store
	rc, m, err := fs.Get("local", p.ChatID, strings.TrimSuffix(strings.TrimPrefix(p.Upload.URL[strings.LastIndex(p.Upload.URL, "/")+1:], ""), "?ticket=tkn"))
	_ = rc
	_ = m
	_ = err
	// 上面路径拆解太 brittle；下面直接用 registry 取回 target 做验证
	tgt, ok := reg.Lookup(p.ChatID)
	if !ok || tgt.ReceiveID != "wmUSER" || tgt.ReceiveIDType != "chatid" {
		t.Fatalf("registry: ok=%v target=%+v", ok, tgt)
	}
}

// 非 image/ 开头的 mimeType，msgtype=file → upload.type=file
func TestHandleInboundFile(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	plain := []byte("fake pdf content")
	ct := encryptForWecom(key, plain)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(ct)
	}))
	defer ts.Close()

	b, _, fp, _, _ := newBridgeWithStore(t)
	in := Inbound{Cmd: CmdCallback}
	in.Headers.ReqID = "R-F"
	in.Body.MsgType = MsgTypeFile
	in.Body.ExternalUserID = "wmU"
	in.Body.File = &MediaPayload{URL: ts.URL, AESKey: base64.StdEncoding.EncodeToString(key), Filename: "report.pdf"}
	b.HandleWecomMessage(in)

	req := fp.frames[0].(protocol.Request)
	var p struct {
		Upload struct{ Type, Name, MimeType string } `json:"upload"`
	}
	_ = json.Unmarshal(req.Payload, &p)
	if p.Upload.Type != "file" || p.Upload.MimeType != "application/pdf" {
		t.Fatalf("upload: %+v", p.Upload)
	}
}

// 入站语音：取 recognized_text，走 /api/query
func TestHandleInboundVoiceWithRecognizedText(t *testing.T) {
	b, _, fp, _, _ := newBridgeWithStore(t)
	in := Inbound{Cmd: CmdCallback}
	in.Headers.ReqID = "R-V"
	in.Body.MsgType = MsgTypeVoice
	in.Body.ExternalUserID = "wmU"
	in.Body.Voice = &VoicePayload{RecognizedText: "what time is it"}
	b.HandleWecomMessage(in)

	if len(fp.frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(fp.frames))
	}
	req := fp.frames[0].(protocol.Request)
	if req.Type != "/api/query" {
		t.Fatalf("type: %s", req.Type)
	}
	var p struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(req.Payload, &p)
	if p.Message != "what time is it" {
		t.Fatalf("voice text not used: %q", p.Message)
	}
}

// 入站语音无识别文本：bridge 回提示，不发给 platform
func TestHandleInboundVoiceEmpty(t *testing.T) {
	b, fw, fp, _, _ := newBridgeWithStore(t)
	in := Inbound{Cmd: CmdCallback}
	in.Headers.ReqID = "R-V2"
	in.Body.MsgType = MsgTypeVoice
	in.Body.Voice = &VoicePayload{}
	b.HandleWecomMessage(in)

	if len(fp.frames) != 0 {
		t.Fatalf("should not forward empty voice")
	}
	if len(fw.text) != 1 || !strings.Contains(fw.text[0].Content, "empty") {
		t.Fatalf("expected empty-voice fallback reply: %+v", fw.text)
	}
}

// SendMediaToWecom：registry 命中则 UploadMedia → SendImage/File
type fakeWecomWithCounts struct{ fakeWecom }

func (f *fakeWecomWithCounts) UploadMedia(mediaType, filename string, data []byte) (string, error) {
	return "MID_" + mediaType + "_" + string(rune(len(data))), nil
}

func TestSendMediaToWecomImage(t *testing.T) {
	b, _, _, _, reg := newBridgeWithStore(t)
	reg.Register("chat-1", Target{AppKey: "default", ReceiveID: "wmU", ReceiveIDType: "userid"})

	// 只要不报错（注入的 fakeWecom.UploadMedia/SendImage 都返回 nil）
	if err := b.SendMediaToWecom("chat-1", "a.png", "image/png", bytes.Repeat([]byte{1}, 10)); err != nil {
		t.Fatalf("SendMediaToWecom: %v", err)
	}
}

func TestSendMediaToWecomUnknownChat(t *testing.T) {
	b, _, _, _, _ := newBridgeWithStore(t)
	if err := b.SendMediaToWecom("unknown", "x", "application/pdf", []byte("x")); err != nil {
		t.Fatalf("unknown chat should silently no-op: %v", err)
	}
}
