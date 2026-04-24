package wecom

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMediaPayloadAliases(t *testing.T) {
	raw := []byte(`{"downloadUrl":"http://x","aes_key":"k","mediaId":"m","name":"a.png","contentType":"image/png"}`)
	var m MediaPayload
	_ = json.Unmarshal(raw, &m)
	if m.ResolveURL() != "http://x" || m.ResolveAESKey() != "k" || m.ResolveName() != "a.png" || m.ResolveMimeType() != "image/png" {
		t.Fatalf("alias resolution failed: %+v", m)
	}
}

func TestVoiceRecognizedTextAliases(t *testing.T) {
	raw := []byte(`{"recognizedText":"hi there"}`)
	var v VoicePayload
	_ = json.Unmarshal(raw, &v)
	if v.ResolveText() != "hi there" {
		t.Fatalf("voice text: %+v", v)
	}
}

func TestUploadFrames(t *testing.T) {
	i := NewUploadInit("u", "image", "a.png", 1000, 2, "md5hex")
	b, _ := json.Marshal(i)
	for _, w := range []string{`"cmd":"aibot_upload_media_init"`, `"total_size":1000`, `"total_chunks":2`, `"md5":"md5hex"`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("missing %s in %s", w, b)
		}
	}
	c := NewUploadChunk("u", "UPID", 0, "BASE64")
	b, _ = json.Marshal(c)
	for _, w := range []string{`"upload_id":"UPID"`, `"chunk_index":0`, `"base64_data":"BASE64"`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("missing %s in %s", w, b)
		}
	}
	f := NewUploadFinish("u", "UPID")
	b, _ = json.Marshal(f)
	if !strings.Contains(string(b), `"upload_id":"UPID"`) {
		t.Errorf("finish: %s", b)
	}
}

func TestSendImageFrame(t *testing.T) {
	m := NewSendImage("u", "wm123", "userid", "MID")
	b, _ := json.Marshal(m)
	s := string(b)
	for _, w := range []string{`"cmd":"aibot_send_msg"`, `"msgtype":"image"`, `"image":{"media_id":"MID"}`, `"userid":"wm123"`} {
		if !strings.Contains(s, w) {
			t.Errorf("missing %s in %s", w, s)
		}
	}
}

func TestSendFileFrameWithChatid(t *testing.T) {
	m := NewSendFile("u", "grp-1", "chatid", "MID")
	b, _ := json.Marshal(m)
	s := string(b)
	if !strings.Contains(s, `"msgtype":"file"`) || !strings.Contains(s, `"chatid":"grp-1"`) {
		t.Errorf("%s", s)
	}
}

func TestAckRichCarriesBody(t *testing.T) {
	raw := []byte(`{"cmd":"aibot_upload_media_finish","errcode":0,"body":{"media_id":"M_1"}}`)
	var a AckRich
	_ = json.Unmarshal(raw, &a)
	if a.Body.MediaID != "M_1" {
		t.Fatalf("ack rich: %+v", a)
	}
}
