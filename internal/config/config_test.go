package config

import (
	"strings"
	"testing"
)

// resetEnv 把会影响 Load() 的 env 全部清掉，避免测试之间串扰。
func resetEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"BRIDGE_HTTP_ADDR", "BRIDGE_STATE_DIR", "BRIDGE_LOG_LEVEL",
		"BRIDGE_AGENT_KEY", "BRIDGE_CHANNEL", "BRIDGE_USER_ID",
		"BRIDGE_PUBLIC_BASE_URL",
		"WECOM_ENABLED", "WECOM_WS_URL", "WECOM_BOT_ID", "WECOM_SECRET",
		"WECOM_APP_KEY", "WECOM_HEARTBEAT_SECONDS",
	}
	for i := 0; i < 5; i++ {
		keys = append(keys,
			"WECOM_BOT_"+itoa(i)+"_ID",
			"WECOM_BOT_"+itoa(i)+"_SECRET",
			"WECOM_BOT_"+itoa(i)+"_APPKEY",
		)
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestLoadBotsFromLegacyEnv(t *testing.T) {
	resetEnv(t)
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

func TestLoadBotsFromIndexedEnv(t *testing.T) {
	resetEnv(t)
	t.Setenv("WECOM_ENABLED", "true")
	t.Setenv("WECOM_BOT_0_ID", "botA")
	t.Setenv("WECOM_BOT_0_SECRET", "sA")
	t.Setenv("WECOM_BOT_0_APPKEY", "xiaozhai")
	t.Setenv("WECOM_BOT_1_ID", "botB")
	t.Setenv("WECOM_BOT_1_SECRET", "sB")
	t.Setenv("WECOM_BOT_1_APPKEY", "assistant")

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

// 缺失 appKey 时用 bot id 兜底，保证多 bot 唯一路由键。
func TestLoadBotsAppKeyFallsBackToBotID(t *testing.T) {
	resetEnv(t)
	t.Setenv("WECOM_ENABLED", "true")
	t.Setenv("WECOM_BOT_0_ID", "bot-xyz")
	t.Setenv("WECOM_BOT_0_SECRET", "s")
	// no _APPKEY

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bots[0].AppKey != "bot-xyz" {
		t.Fatalf("appKey fallback: got %q want bot-xyz", cfg.Bots[0].AppKey)
	}
}

// 索引 env 的扫描在首个缺失 _ID 的 index 处停止。
func TestLoadBotsIndexedStopsAtGap(t *testing.T) {
	resetEnv(t)
	t.Setenv("WECOM_ENABLED", "true")
	t.Setenv("WECOM_BOT_0_ID", "botA")
	t.Setenv("WECOM_BOT_0_SECRET", "sA")
	// BOT_1 缺省
	t.Setenv("WECOM_BOT_2_ID", "botC") // 这条应当被跳过
	t.Setenv("WECOM_BOT_2_SECRET", "sC")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Bots) != 1 {
		t.Fatalf("scanner should stop at BOT_1 gap, got %d bots", len(cfg.Bots))
	}
}

func TestLoadBotsDuplicateAppKeyRejected(t *testing.T) {
	resetEnv(t)
	t.Setenv("WECOM_ENABLED", "true")
	t.Setenv("WECOM_BOT_0_ID", "botA")
	t.Setenv("WECOM_BOT_0_SECRET", "sA")
	t.Setenv("WECOM_BOT_0_APPKEY", "dup")
	t.Setenv("WECOM_BOT_1_ID", "botB")
	t.Setenv("WECOM_BOT_1_SECRET", "sB")
	t.Setenv("WECOM_BOT_1_APPKEY", "dup")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "duplicate appKey") {
		t.Fatalf("expected duplicate appKey error, got %v", err)
	}
}

func TestLoadBotsMissingCredentialsRejected(t *testing.T) {
	resetEnv(t)
	t.Setenv("WECOM_ENABLED", "true")
	_, err := Load()
	if err == nil {
		t.Fatalf("expected error when wecom enabled but no creds")
	}
}

// 索引式 bot 有 _ID 没 _SECRET 要 fail-fast（防止意外跳过）。
func TestLoadBotsIndexedMissingSecretRejected(t *testing.T) {
	resetEnv(t)
	t.Setenv("WECOM_ENABLED", "true")
	t.Setenv("WECOM_BOT_0_ID", "botA")
	// no _SECRET

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("expected SECRET-missing error, got %v", err)
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
