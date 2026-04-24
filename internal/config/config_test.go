package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetEnv 把会影响 Load() 的 env 全部清掉，避免测试之间串扰。
func resetEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"BRIDGE_HTTP_ADDR", "BRIDGE_STATE_DIR", "BRIDGE_LOG_LEVEL",
		"BRIDGE_AGENT_KEY", "BRIDGE_CHANNEL", "BRIDGE_USER_ID",
		"BRIDGE_PUBLIC_BASE_URL", "BRIDGE_BOTS_FILE",
		"WECOM_ENABLED", "WECOM_WS_URL", "WECOM_BOT_ID", "WECOM_SECRET",
		"WECOM_APP_KEY", "WECOM_HEARTBEAT_SECONDS",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

func TestLoadBotsFromLegacyEnv(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	t.Setenv("BRIDGE_STATE_DIR", dir)
	t.Setenv("WECOM_ENABLED", "true")
	t.Setenv("WECOM_BOT_ID", "bot-legacy")
	t.Setenv("WECOM_SECRET", "sec-legacy")
	t.Setenv("WECOM_APP_KEY", "xiaozhai")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Bots) != 1 {
		t.Fatalf("want 1 bot from legacy env, got %d", len(cfg.Bots))
	}
	b := cfg.Bots[0]
	if b.ID != "bot-legacy" || b.Secret != "sec-legacy" || b.AppKey != "xiaozhai" {
		t.Fatalf("bot fields mismatched: %+v", b)
	}
}

func TestLoadBotsFromFile(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	botsPath := filepath.Join(dir, "bots.json")
	json := `[
		{"id":"botA","secret":"sA","appKey":"xiaozhai"},
		{"id":"botB","secret":"sB","appKey":"assistant"}
	]`
	if err := os.WriteFile(botsPath, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_STATE_DIR", dir)
	t.Setenv("WECOM_ENABLED", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Bots) != 2 {
		t.Fatalf("want 2 bots, got %d: %+v", len(cfg.Bots), cfg.Bots)
	}
	if cfg.Bots[0].AppKey != "xiaozhai" || cfg.Bots[1].AppKey != "assistant" {
		t.Fatalf("appKeys wrong: %+v", cfg.Bots)
	}
}

func TestLoadBotsDuplicateAppKeyRejected(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	botsPath := filepath.Join(dir, "bots.json")
	json := `[
		{"id":"botA","secret":"sA","appKey":"dup"},
		{"id":"botB","secret":"sB","appKey":"dup"}
	]`
	_ = os.WriteFile(botsPath, []byte(json), 0o600)
	t.Setenv("BRIDGE_STATE_DIR", dir)
	t.Setenv("WECOM_ENABLED", "true")

	_, err := Load()
	if err == nil {
		t.Fatalf("expected duplicate appKey error")
	}
	if !strings.Contains(err.Error(), "duplicate appKey") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadBotsMissingCredentialsRejected(t *testing.T) {
	resetEnv(t)
	t.Setenv("WECOM_ENABLED", "true")
	// no bots.json, no legacy creds
	_, err := Load()
	if err == nil {
		t.Fatalf("expected error when wecom enabled but no creds")
	}
}

func TestLoadBotsDisabledWhenWecomOff(t *testing.T) {
	resetEnv(t)
	t.Setenv("WECOM_ENABLED", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Bots) != 0 {
		t.Fatalf("want no bots when wecom disabled, got %d", len(cfg.Bots))
	}
}
