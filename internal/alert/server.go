package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/your-org/ai-sre/internal/config"
)

// Alert — входящий алерт от Alertmanager
type Alert struct {
	Name        string            `json:"alertname"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Status      string            `json:"status"`
	StartsAt    time.Time         `json:"startsAt"`
	Fingerprint string            `json:"fingerprint"`
}

// RunbookURL возвращает URL runbook из annotations если есть
func (a *Alert) RunbookURL() string {
	if u, ok := a.Annotations["runbook_url"]; ok {
		return u
	}
	return ""
}

// DeduplicationKey — ключ для дедупликации похожих алертов
func (a *Alert) DeduplicationKey() string {
	return fmt.Sprintf("%s/%s", a.Name, a.Namespace)
}

// alertmanagerPayload — raw payload от Alertmanager
type alertmanagerPayload struct {
	Alerts []struct {
		Status string            `json:"status"`
		Labels map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    time.Time         `json:"startsAt"`
		Fingerprint string            `json:"fingerprint"`
	} `json:"alerts"`
}

// Investigator — интерфейс агента (чтобы не импортировать циклически)
type Investigator interface {
	Investigate(ctx context.Context, alert *Alert) error
}

type ServerDeps struct {
	Agent  Investigator
	Logger *slog.Logger
	Config config.ServerConfig
}

type Server struct {
	deps     ServerDeps
	router   *chi.Mux
	httpSrv  *http.Server
	inflight sync.Map // dedup map: key → struct{}
}

func NewServer(deps ServerDeps) *Server {
	s := &Server{deps: deps}
	s.router = chi.NewRouter()
	s.router.Use(middleware.Recoverer)
	s.router.Use(middleware.RequestID)
	s.router.Post("/webhook", s.handleWebhook)
	s.router.Get("/healthz", s.handleHealth)
	s.httpSrv = &http.Server{
		Addr:    deps.Config.Addr,
		Handler: s.router,
	}
	return s
}

func (s *Server) Run() error {
	return s.httpSrv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) {
	_ = s.httpSrv.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var payload alertmanagerPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.deps.Logger.Error("failed to decode payload", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Группируем алерты: берём первый firing как главный,
	// остальные добавляем в контекст
	alert := s.extractPrimary(payload)
	if alert == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Дедупликация: если расследование уже идёт — игнорируем
	key := alert.DeduplicationKey()
	if _, loaded := s.inflight.LoadOrStore(key, struct{}{}); loaded {
		s.deps.Logger.Info("investigation already in progress, skipping",
			"key", key)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Запускаем расследование асинхронно
	go func() {
		defer s.inflight.Delete(key)
		ctx := context.Background()
		if err := s.deps.Agent.Investigate(ctx, alert); err != nil {
			s.deps.Logger.Error("investigation failed", "err", err, "alert", alert.Name)
		}
	}()

	w.WriteHeader(http.StatusOK)
}

// extractPrimary — берём первый firing алерт из группы как главный
func (s *Server) extractPrimary(payload alertmanagerPayload) *Alert {
	for _, a := range payload.Alerts {
		if a.Status != "firing" {
			continue
		}
		return &Alert{
			Name:        a.Labels["alertname"],
			Namespace:   a.Labels["namespace"],
			Labels:      a.Labels,
			Annotations: a.Annotations,
			Status:      a.Status,
			StartsAt:    a.StartsAt,
			Fingerprint: a.Fingerprint,
		}
	}
	return nil
}
