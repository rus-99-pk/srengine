package alert

import (
	"context"
	"encoding/json"
	// "fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rus-99-pk/srengine/internal/config"
)

type Alert struct {
	Name        string            `json:"alertname"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Status      string            `json:"status"`
	StartsAt    time.Time         `json:"startsAt"`
	Fingerprint string            `json:"fingerprint"`
}

func (a *Alert) RunbookURL() string {
	if u, ok := a.Annotations["runbook_url"]; ok {
		return u
	}
	return ""
}

type alertmanagerPayload struct {
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    time.Time         `json:"startsAt"`
		Fingerprint string            `json:"fingerprint"`
	} `json:"alerts"`
}

// ВАЖНО: Мы изменили сигнатуру с error на (any, error)
type Investigator interface {
	Investigate(ctx context.Context, alert *Alert) (any, error)
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
	inflight sync.Map // fingerprint -> struct{}
	results  sync.Map // fingerprint -> *agent.Report (передаем как any)
}

func NewServer(deps ServerDeps) *Server {
	s := &Server{deps: deps}
	s.router = chi.NewRouter()
	s.router.Use(middleware.Recoverer)
	s.router.Use(middleware.RequestID)
	
	s.router.Post("/webhook", s.handleWebhook)
	s.router.Get("/result", s.handleResult) // <-- ДОБАВИЛИ ЭНДПОИНТ
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

	alert := s.extractPrimary(payload)
	if alert == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Дедупликация и ключ для тестов - Fingerprint
	fp := alert.Fingerprint
	if _, loaded := s.inflight.LoadOrStore(fp, struct{}{}); loaded {
		s.deps.Logger.Info("investigation already in progress, skipping", "fingerprint", fp)
		w.WriteHeader(http.StatusOK)
		return
	}

	go func() {
		defer s.inflight.Delete(fp)
		ctx := context.Background()
		
		// Сохраняем результат в кэш сервера после расследования
		res, err := s.deps.Agent.Investigate(ctx, alert)
		if err != nil {
			s.deps.Logger.Error("investigation failed", "err", err, "alert", alert.Name)
		}
		if res != nil {
			s.results.Store(fp, res)
		}
	}()

	w.WriteHeader(http.StatusOK)
}

// ДОБАВИЛИ ОБРАБОТЧИК /result ДЛЯ ТЕСТОВ
func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	fp := r.URL.Query().Get("fingerprint")

	// 1. Проверяем, есть ли готовый результат
	if res, ok := s.results.Load(fp); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(res)
		return
	}

	// 2. Проверяем, идет ли еще расследование
	if _, ok := s.inflight.Load(fp); ok {
		w.WriteHeader(http.StatusAccepted) // 202
		return
	}

	// 3. Расследование не найдено
	w.WriteHeader(http.StatusNotFound) // 404
}

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