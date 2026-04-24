package server

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutAndGet(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	body := bytes.NewBufferString("hello world")
	meta := Meta{Name: "greeting.txt", MimeType: "text/plain", SizeBytes: 11, SHA256: "abc"}
	info, err := s.Put("u1", "wecom#single#src#1a", "file-1", body, meta)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if info.SHA256 == "" || info.SizeBytes != 11 || info.Name != "greeting.txt" {
		t.Fatalf("unexpected info: %+v", info)
	}

	rc, got, err := s.Get("u1", "wecom#single#src#1a", "file-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "hello world" {
		t.Fatalf("bytes mismatch: %q", b)
	}
	if got.Name != meta.Name || got.MimeType != meta.MimeType || got.SizeBytes != 11 {
		t.Fatalf("meta mismatch: %+v", got)
	}
}

func TestGetMissing(t *testing.T) {
	s, _ := NewFileStore(t.TempDir())
	_, _, err := s.Get("u1", "chatid", "missing")
	if err == nil {
		t.Fatal("expected ErrNotFound")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected err: %v", err)
	}
}

func TestSHAComputedWhenMissing(t *testing.T) {
	s, _ := NewFileStore(t.TempDir())
	info, _ := s.Put("u1", "chat1", "f1", strings.NewReader("abc"), Meta{Name: "a.txt", MimeType: "text/plain"})
	if info.SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("SHA256 mismatch: %s", info.SHA256)
	}
	if info.SizeBytes != 3 {
		t.Fatalf("size mismatch: %d", info.SizeBytes)
	}
}
