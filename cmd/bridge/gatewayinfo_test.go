package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-wecom-bridge/internal/config"
)

func TestHandleGatewayInfoResponseShape(t *testing.T) {
	cfg := config.Config{
		AgentKey: "zenmi",
		Channel:  "wecom:xiaozhai",
		Bots:     []config.BotConfig{{ID: "botAAA", Secret: "s", AppKey: "xiaozhai"}},
	}
	h := handleGatewayInfo(cfg, "jwt-token-xyz")

	req := httptest.NewRequest(http.MethodGet, "/gateway/info", nil)
	req.Host = "127.0.0.1:11970"
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if body["id"] != "wecom-botAAA" {
		t.Errorf("id: got %v want wecom-botAAA", body["id"])
	}
	if body["channel"] != "wecom" {
		t.Errorf("channel: got %v want wecom", body["channel"])
	}
	if body["token"] != "jwt-token-xyz" {
		t.Errorf("token: got %v", body["token"])
	}
	if body["baseUrl"] != "http://127.0.0.1:11970" {
		t.Errorf("baseUrl: got %v", body["baseUrl"])
	}
	url, _ := body["url"].(string)
	if !strings.Contains(url, "agentKey=zenmi") || !strings.Contains(url, "channel=wecom:xiaozhai") {
		t.Errorf("url missing params: %v", url)
	}
}

func TestHandleGatewayInfoIDFallbackWhenBotIDEmpty(t *testing.T) {
	cfg := config.Config{Channel: "feishu:p2p", AgentKey: "agent"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/gateway/info", nil)
	req.Host = "localhost:11970"
	handleGatewayInfo(cfg, "tok")(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["id"] != "bridge-feishu" {
		t.Errorf("id fallback: got %v want bridge-feishu", body["id"])
	}
	if body["channel"] != "feishu" {
		t.Errorf("channel: got %v", body["channel"])
	}
}

func TestLoopbackOnlyBlocksExternal(t *testing.T) {
	h := loopbackOnly(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	cases := map[string]int{
		"127.0.0.1:1234": http.StatusOK,
		"[::1]:1234":     http.StatusOK,
		"8.8.8.8:1234":   http.StatusForbidden,
		"10.0.0.5:1234":  http.StatusForbidden,
	}
	for remote, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != want {
			t.Errorf("RemoteAddr=%s: got %d want %d", remote, rec.Code, want)
		}
	}
}
