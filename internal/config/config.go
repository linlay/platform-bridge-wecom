// Package config loads bridge runtime configuration from environment.
//
// Phase 1 只包含最小启动所需的字段；wecom / platform / ticket 相关字段在
// Phase 2+ 按需扩展。字段语义见仓库根目录 .env.example。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	HTTPAddr        string
	StateDir        string
	LogLevel        string
	ShutdownTimeout time.Duration

	// 对接 platform 的身份。归一化规则与 ticket 包保持一致（ticket 内部自己 lowercase）。
	AgentKey string
	Channel  string
	UserID   string

	// 可选外部公告 URL，用于 /api/upload 帧里的 url host 部分（不填则用 Host header）。
	PublicBaseURL string

	// 企微 Bot 连接参数（Phase 3）
	WecomEnabled      bool
	WecomWSURL        string
	WecomBotID        string
	WecomSecret       string
	WecomAppKey       string
	WecomHeartbeatSec int
}

// Load reads .env (if present) then parses env vars into Config. Missing
// optional fields fall back to sensible defaults; required fields return an
// error.
func Load() (Config, error) {
	_ = godotenv.Load() // 允许无 .env 运行（生产用真实环境变量）

	shutdown, err := parseSeconds("BRIDGE_SHUTDOWN_TIMEOUT_SECONDS", 10)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		HTTPAddr:        getOr("BRIDGE_HTTP_ADDR", ":11970"),
		StateDir:        getOr("BRIDGE_STATE_DIR", "./runtime"),
		LogLevel:        strings.ToLower(getOr("BRIDGE_LOG_LEVEL", "info")),
		ShutdownTimeout: shutdown,
		AgentKey:        getOr("BRIDGE_AGENT_KEY", "personal"),
		Channel:         getOr("BRIDGE_CHANNEL", "wecom:personal"),
		UserID:          getOr("BRIDGE_USER_ID", "local"),
		PublicBaseURL:   getOr("BRIDGE_PUBLIC_BASE_URL", ""),

		WecomEnabled:      strings.EqualFold(getOr("WECOM_ENABLED", "false"), "true"),
		WecomWSURL:        getOr("WECOM_WS_URL", "wss://openws.work.weixin.qq.com"),
		WecomBotID:        getOr("WECOM_BOT_ID", ""),
		WecomSecret:       getOr("WECOM_SECRET", ""),
		WecomAppKey:       getOr("WECOM_APP_KEY", "default"),
		WecomHeartbeatSec: atoiOr("WECOM_HEARTBEAT_SECONDS", 30),
	}

	if err := rejectDeprecated(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func getOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func parseSeconds(key string, fallback int) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return time.Duration(fallback) * time.Second, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid %s=%q: expected positive integer seconds", key, raw)
	}
	return time.Duration(n) * time.Second, nil
}

// rejectDeprecated 在发现已废弃的 env 变量时 fail-fast，提醒用户更新 .env。
// Phase 1 暂无历史包袱，留空占位。
func rejectDeprecated() error {
	return nil
}

func atoiOr(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
