package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"agent-wecom-bridge/internal/config"
	"agent-wecom-bridge/internal/diag"
	"agent-wecom-bridge/internal/protocol"
	"agent-wecom-bridge/internal/server"
	"agent-wecom-bridge/internal/wecom"
)

// platformSenderAdapter 把 server.Broadcast 适配成 wecom.PlatformSender 接口。
type platformSenderAdapter struct{ ws *server.WSServer }

func (p *platformSenderAdapter) SendFrame(v any) error { return p.ws.Broadcast(v) }

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	diag.Configure(cfg.LogLevel)
	diag.Info("bridge.config.loaded",
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
