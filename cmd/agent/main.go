package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/rus-99-pk/srengine/internal/agent"
	"github.com/rus-99-pk/srengine/internal/alert"
	"github.com/rus-99-pk/srengine/internal/config"
	"github.com/rus-99-pk/srengine/internal/k8s"
	"github.com/rus-99-pk/srengine/internal/llm"
	"github.com/rus-99-pk/srengine/internal/logs"
	"github.com/rus-99-pk/srengine/internal/notifier"
	"github.com/rus-99-pk/srengine/internal/tools"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// K8s client
	k8sClient, err := k8s.NewClient(cfg.Namespaces)
	if err != nil {
		logger.Error("failed to create k8s client", "err", err)
		os.Exit(1)
	}

	// Check namespace access on startup
	ctx := context.Background()
	accessReport := k8sClient.CheckAccess(ctx)
	for ns, status := range accessReport {
		logger.Info("namespace access", "namespace", ns, "status", status)
	}

	// LLM provider
	llmProvider, err := llm.NewOllamaProvider(cfg.LLM)
	if err != nil {
		logger.Error("failed to create llm provider", "err", err)
		os.Exit(1)
	}

	// Log deduplicator
	deduplicator := logs.NewDeduplicator(cfg.Logs)

	// Tool registry
	registry := tools.NewRegistry(k8sClient, deduplicator, cfg)

	// Notifier
	n, err := notifier.New(cfg.Notifier)
	if err != nil {
		logger.Error("failed to create notifier", "err", err)
		os.Exit(1)
	}

	// Agent
	a := agent.New(agent.Deps{
		LLM:      llmProvider,
		Tools:    registry,
		Notifier: n,
		Logger:   logger,
		Config:   cfg.Agent,
	})

	// Alert webhook server
	server := alert.NewServer(alert.ServerDeps{
		Agent:  a,
		Logger: logger,
		Config: cfg.Server,
	})

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("starting webhook server", "addr", cfg.Server.Addr)
		if err := server.Run(); err != nil {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-quit
	logger.Info("shutting down")
	server.Shutdown(ctx)
}
