package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agent-wecom-bridge/internal/config"
	"agent-wecom-bridge/internal/diag"
	"agent-wecom-bridge/internal/protocol"
	"agent-wecom-bridge/internal/server"
	"agent-wecom-bridge/internal/wecom"
)

// 由 ldflags 注入（见 scripts/build-release.sh）。dev 构建为占位值。
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

// platformSenderAdapter 把 server.Broadcast 适配成 wecom.PlatformSender 接口。
type platformSenderAdapter struct{ ws *server.WSServer }

func (p *platformSenderAdapter) SendFrame(v any) error { return p.ws.Broadcast(v) }

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-v" || os.Args[1] == "--version" || os.Args[1] == "version") {
		log.Printf("platform-bridge-wecom %s (commit %s, built %s)", version, commit, buildTime)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	diag.Configure(cfg.LogLevel)
	diag.Info("bridge.config.loaded",
		"version", version,
		"commit", commit,
		"http_addr", cfg.HTTPAddr,
		"state_dir", cfg.StateDir,
		"agent_key", cfg.AgentKey,
		"channel", cfg.Channel,
		"user_id", cfg.UserID,
		"wecom_enabled", cfg.WecomEnabled,
	)

	fs, err := server.NewFileStore(filepath.Join(cfg.StateDir, "blob"))
	if err != nil {
		log.Fatalf("store init: %v", err)
	}

	reg, err := wecom.OpenRegistry(filepath.Join(cfg.StateDir, "wecom-chatid.json"))
	if err != nil {
		log.Fatalf("registry: %v", err)
	}

	tk, err := protocol.IssueTicket(cfg.Channel, cfg.UserID, cfg.AgentKey)
	if err != nil {
		log.Fatalf("ticket issue: %v", err)
	}
	diag.Info("bridge.ticket.issued", "ticket", tk)
	log.Printf("\n=========================================\n"+
		"GATEWAY_WS_URL=ws://<host>%s/ws/agent?agentKey=%s&channel=%s\n"+
		"GATEWAY_JWT_TOKEN=%s\n"+
		"=========================================\n",
		cfg.HTTPAddr, cfg.AgentKey, cfg.Channel, tk)

	// bridge：注入 Platform sender（通过 ws 指针间接）；Wecom 稍后注入
	psender := &platformSenderAdapter{}
	br := wecom.NewBridge(wecom.BridgeConfig{
		AppKey:         cfg.WecomAppKey,
		Channel:        cfg.Channel,
		AgentKey:       cfg.AgentKey,
		Platform:       psender,
		Registry:       reg,
		Dedup:          wecom.NewDedup(10 * time.Minute),
		Store:          fs,
		UserID:         cfg.UserID,
		DownloadTicket: tk,
	})

	api := server.New(server.Config{
		Channel:  cfg.Channel,
		AgentKey: cfg.AgentKey,
		UserID:   cfg.UserID,
		Store:    fs,
		OnPushed: func(chatID, name, mimeType string, data []byte) error {
			return br.SendMediaToWecom(chatID, name, mimeType, data)
		},
	})

	ws := server.NewWSServer(server.WSConfig{
		Channel:  cfg.Channel,
		AgentKey: cfg.AgentKey,
		UserID:   cfg.UserID,
		OnFrame: func(sessionID string, env protocol.Envelope) {
			br.HandlePlatformFrame(sessionID, env)
		},
	})
	psender.ws = ws

	var wcli *wecom.Client
	if cfg.WecomEnabled {
		if cfg.WecomBotID == "" || cfg.WecomSecret == "" {
			log.Fatalf("WECOM_ENABLED=true but WECOM_BOT_ID / WECOM_SECRET is empty")
		}
		wcli = wecom.NewClient(wecom.ClientConfig{
			URL:               cfg.WecomWSURL,
			BotID:             cfg.WecomBotID,
			Secret:            cfg.WecomSecret,
			HeartbeatInterval: time.Duration(cfg.WecomHeartbeatSec) * time.Second,
			ReconnectMin:      time.Second,
			ReconnectMax:      30 * time.Second,
			OnReady:           func() { diag.Info("bridge.wecom.ready") },
			OnMessage: func(in wecom.Inbound) {
				br.HandleWecomMessage(in)
			},
		})
		br.SetWecom(wcli)
		// StreamSender 依赖 wcli 的 SendMarkdown（aibot_respond_msg stream），
		// 必须在 wcli 就绪后构造；对齐 Java WecomMessageSender 的流式路径。
		br.SetStream(wecom.NewStreamSender(wcli))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ws/agent", ws.HandleAgentWS)
	mux.Handle("/api/push", api.Router())
	mux.Handle("/api/download/", api.Router())
	// /gateway/info: desktop 拉到本插件的 platform 注册信息（id/channel/url/token/baseUrl），
	// 再 POST 到 platform 的 /api/admin/gateways。只接受 loopback，避免外网拿到 JWT。
	mux.HandleFunc("/gateway/info", loopbackOnly(handleGatewayInfo(cfg, tk)))

	httpSrv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if wcli != nil {
		go wcli.Run(shutdownCtx)
	}

	go func() {
		diag.Info("bridge.http.listen", "addr", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http listen: %v", err)
		}
	}()

	<-shutdownCtx.Done()
	diag.Info("bridge.shutdown.start")
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	diag.Info("bridge.shutdown.complete")
}

// handleGatewayInfo 返回 desktop 注册这个 bridge 到 platform 所需的字段。
// 形状与 platform /api/admin/gateways POST body 一致，desktop 直接透传。
//
//   - id:      gateway 在 platform 里的唯一键，默认 "{channel}-{botId}"，否则 "bridge-{channel}"
//   - channel: 路由前缀（"wecom" / "feishu" / ...），从 BRIDGE_CHANNEL 的 "wecom:xxx" 取冒号前
//   - url:     platform 反向连接用的 ws URL（host 从 Host header 推导，scheme 默认 ws）
//   - token:   bridge 签发的 JWT，platform 握手用 Bearer
//   - baseUrl: http(s):// 版本，platform 回取 artifact / 下载用户文件时用
func handleGatewayInfo(cfg config.Config, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		channel := cfg.Channel
		if idx := strings.Index(channel, ":"); idx > 0 {
			channel = channel[:idx]
		}

		id := strings.TrimSpace(cfg.WecomBotID)
		if id != "" {
			id = channel + "-" + id
		} else {
			id = "bridge-" + channel
		}

		host := r.Host
		if cfg.PublicBaseURL != "" {
			host = strings.TrimPrefix(strings.TrimPrefix(cfg.PublicBaseURL, "http://"), "https://")
			host = strings.TrimSuffix(host, "/")
		}
		wsURL := "ws://" + host + "/ws/agent?agentKey=" + cfg.AgentKey + "&channel=" + cfg.Channel
		baseURL := "http://" + host

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      id,
			"channel": channel,
			"url":     wsURL,
			"token":   token,
			"baseUrl": baseURL,
		})
	}
}

// loopbackOnly 限制 handler 只接受 127.0.0.1 / ::1 请求。
// desktop 和 bridge 一定同机，外网来的请求必然是误触（或攻击）。
func loopbackOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.RemoteAddr
		if idx := strings.LastIndex(host, ":"); idx > 0 {
			host = host[:idx]
		}
		host = strings.Trim(host, "[]")
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
