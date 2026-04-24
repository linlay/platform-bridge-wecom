package wecom

import (
	"path/filepath"
	"testing"
)

func TestRegisterAndLookup(t *testing.T) {
	r, err := OpenRegistry(filepath.Join(t.TempDir(), "reg.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.Register("wecom#single#wmX#1a", Target{AppKey: "default", ReceiveID: "wmX", ReceiveIDType: "userid"})
	got, ok := r.Lookup("wecom#single#wmX#1a")
	if !ok || got.AppKey != "default" || got.ReceiveID != "wmX" || got.ReceiveIDType != "userid" {
		t.Fatalf("lookup: ok=%v target=%+v", ok, got)
	}
	if _, ok := r.Lookup("missing"); ok {
		t.Fatal("missing key should not hit")
	}
}

func TestPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.json")
	r1, _ := OpenRegistry(path)
	r1.Register("cid", Target{AppKey: "ak", ReceiveID: "r", ReceiveIDType: "chatid"})

	r2, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r2.Lookup("cid")
	if !ok || got.ReceiveID != "r" {
		t.Fatalf("persisted lookup: %+v ok=%v", got, ok)
	}
}
