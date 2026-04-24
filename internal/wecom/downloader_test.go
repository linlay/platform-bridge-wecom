package wecom

import (
	"bytes"
	"crypto/aes"
	ccipher "crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 构造 Java 风格的 AES 密文（帮手）
func encryptRef(t *testing.T, key, plain []byte) []byte {
	t.Helper()
	block, _ := aes.NewCipher(key)
	iv := key[:16]
	pad := 16 - (len(plain) % 16)
	buf := make([]byte, 0, len(plain)+pad)
	buf = append(buf, plain...)
	for i := 0; i < pad; i++ {
		buf = append(buf, byte(pad))
	}
	ct := make([]byte, len(buf))
	ccipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, buf)
	return ct
}

func TestDownloadAndDecrypt(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	plain := []byte("this is a secret document body")
	ct := encryptRef(t, key, plain)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `attachment; filename="shot.png"`)
		_, _ = w.Write(ct)
	}))
	defer ts.Close()

	res, err := Fetch(ts.URL, base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(res.Bytes, plain) {
		t.Fatalf("bytes: got %q", res.Bytes)
	}
	want := hex.EncodeToString(sha256.New().Sum(nil))
	_ = want
	h := sha256.Sum256(plain)
	if res.SHA256 != hex.EncodeToString(h[:]) {
		t.Fatalf("sha256 mismatch")
	}
	if !strings.HasPrefix(res.ContentType, "image/png") {
		t.Fatalf("ct: %s", res.ContentType)
	}
	if res.Filename != "shot.png" {
		t.Fatalf("filename: %s", res.Filename)
	}
}

// 无 aeskey：原样返回
func TestDownloadNoKey(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("plain body"))
	}))
	defer ts.Close()
	res, err := Fetch(ts.URL, "")
	if err != nil || string(res.Bytes) != "plain body" {
		t.Fatalf("noKey: %v %s", err, res.Bytes)
	}
}

// 非 2xx：返回错误
func TestDownloadError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()
	if _, err := Fetch(ts.URL, ""); err == nil {
		t.Fatal("expected error on 500")
	}
}
