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
	// Structured JSON logger — compatible with k8s log aggregators
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// K8s client with InClusterConfig fallback to KUBECONFIG
	k8sClient, err := k8s.NewClient(cfg.Namespaces)
	if err != nil {
		logger.Error("failed to create k8s client", "err", err)
		os.Exit(1)
	}

	// Verify namespace access on startup and log results
	ctx := context.Background()
	accessReport := k8sClient.CheckAccess(ctx)
	for ns, status := range accessReport {
		logger.Info("namespace access", "namespace", ns, "status", status)
	}

	// Ollama LLM provider
	llmProvider, err := llm.NewOllamaProvider(cfg.LLM)
	if err != nil {
		logger.Error("failed to create llm provider", "err", err)
		os.Exit(1)
	}

	// Log deduplicator using Drain-like algorithm
	deduplicator := logs.NewDeduplicator(cfg.Logs)

	// Tool registry with all available investigation tools
	registry := tools.NewRegistry(k8sClient, deduplicator, cfg)

	// Notifier — stdout fallback if no channels configured
	n, err := notifier.New(cfg.Notifier)
	if err != nil {
		logger.Error("failed to create notifier", "err", err)
		os.Exit(1)
	}

	// Agent wires LLM, tools, notifier and config together
	a := agent.New(agent.Deps{
		LLM:      llmProvider,
		Tools:    registry,
		Notifier: n,
		Logger:   logger,
		Config:   cfg.Agent,
	})

	// HTTP server handles Alertmanager webhooks
	server := alert.NewServer(alert.ServerDeps{
		Agent:  a,
		Logger: logger,
		Config: cfg.Server,
	})

	// Graceful shutdown on SIGINT / SIGTERM
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