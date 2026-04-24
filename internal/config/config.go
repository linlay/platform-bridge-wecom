// Package config loads bridge runtime configuration from environment.
//
// Phase 1 只包含最小启动所需的字段；wecom / platform / ticket 相关字段在
// Phase 2+ 按需扩展。字段语义见仓库根目录 .env.example。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	WecomBotID        string // legacy 单 bot 字段；多 bot 下退化为 Bots[0] 的便利副本
	WecomSecret       string
	WecomAppKey       string
	WecomHeartbeatSec int

	// Bots 支持一个 bridge 进程连接多个企微 bot（N 个 ws 订阅并存）。
	// 载入顺序：优先读 bots.json，否则从 legacy 单 bot env (WECOM_BOT_ID/SECRET/APP_KEY) 合成一条。
	// appKey 必须在 bots 间唯一——chatId 反查路由依赖它区分 bot。
	Bots []BotConfig
}

// BotConfig 单个企微 bot 的凭证 + 标识。AppKey 是 bridge 内部路由键，chatId
// 反查表靠它把 outbound 消息发回对应 bot。AppKey 不会透给企微服务端。
type BotConfig struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
	AppKey string `json:"appKey"`
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

	bots, err := loadBots(cfg)
	if err != nil {
		return Config{}, err
	}
	cfg.Bots = bots

	return cfg, nil
}

// loadBots 解析 bots.json，失败退回 legacy 单 bot env。
// 路径优先级：BRIDGE_BOTS_FILE > $StateDir/bots.json。
// WecomEnabled=false 时返回 nil（Phase 2 或不跑 wecom 的单元测试场景）。
func loadBots(cfg Config) ([]BotConfig, error) {
	if !cfg.WecomEnabled {
		return nil, nil
	}

	path := strings.TrimSpace(os.Getenv("BRIDGE_BOTS_FILE"))
	if path == "" {
		path = filepath.Join(cfg.StateDir, "bots.json")
	}

	if data, err := os.ReadFile(path); err == nil {
		var bots []BotConfig
		if err := json.Unmarshal(data, &bots); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		return validateBots(bots, path)
	}

	// legacy 单 bot env：WECOM_BOT_ID + WECOM_SECRET 合成一条
	if cfg.WecomBotID == "" || cfg.WecomSecret == "" {
		return nil, fmt.Errorf("WECOM_ENABLED=true but no bot credentials: set bots.json or WECOM_BOT_ID+WECOM_SECRET")
	}
	appKey := cfg.WecomAppKey
	if appKey == "" {
		appKey = "default"
	}
	return []BotConfig{{ID: cfg.WecomBotID, Secret: cfg.WecomSecret, AppKey: appKey}}, nil
}

func validateBots(bots []BotConfig, path string) ([]BotConfig, error) {
	if len(bots) == 0 {
		return nil, fmt.Errorf("%s is empty: need at least one bot", path)
	}
	seenKeys := map[string]int{}
	seenIDs := map[string]int{}
	for i, b := range bots {
		b.ID = strings.TrimSpace(b.ID)
		b.Secret = strings.TrimSpace(b.Secret)
		b.AppKey = strings.TrimSpace(b.AppKey)
		if b.ID == "" || b.Secret == "" {
			return nil, fmt.Errorf("%s[%d]: id and secret required", path, i)
		}
		if b.AppKey == "" {
			b.AppKey = b.ID // 未指定时用 bot id 兜底，保证唯一
		}
		if prev, dup := seenKeys[b.AppKey]; dup {
			return nil, fmt.Errorf("%s: duplicate appKey %q at entries [%d] and [%d]", path, b.AppKey, prev, i)
		}
		seenKeys[b.AppKey] = i
		if prev, dup := seenIDs[b.ID]; dup {
			return nil, fmt.Errorf("%s: duplicate bot id %q at entries [%d] and [%d]", path, b.ID, prev, i)
		}
		seenIDs[b.ID] = i
		bots[i] = b
	}
	return bots, nil
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
