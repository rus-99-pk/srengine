package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Namespaces []string
	Agent      AgentConfig
	LLM        LLMConfig
	Logs       LogsConfig
	Server     ServerConfig
	Notifier   NotifierConfig
	Metrics    MetricsConfig

}

type MetricsConfig struct {
	PrometheusURL string
	Timeout       time.Duration
}

type AgentConfig struct {
	MaxSteps        int
	StepTimeout     time.Duration
	InvestigTimeout time.Duration
	PromptPath      string
	SummarizeEvery  int // сжимать контекст каждые N шагов (0 = выключено)
}

type LLMConfig struct {
	Provider   string // ollama
	OllamaURL  string
	Model      string
	Timeout    time.Duration
	MaxRetries int
}

type LogsConfig struct {
	MaxLines      int
	MaxPatterns   int
	Levels        []string // ERROR, WARN
}

type ServerConfig struct {
	Addr string
}

type NotifierConfig struct {
	Telegram TelegramConfig
	Email    EmailConfig
	Webhook  WebhookConfig
}

type TelegramConfig struct {
	Enabled bool
	Token   string
	ChatID  string
}

type EmailConfig struct {
	Enabled  bool
	SMTPHost string
	SMTPPort int
	From     string
	To       []string // несколько получателей
	Password string
}

type WebhookConfig struct {
	Enabled bool
	URL     string
	// опционально: BasicAuth, кастомные заголовки
	Headers map[string]string
	Timeout time.Duration
}

func Load() (*Config, error) {
	cfg := &Config{}

	// Namespaces — через запятую
	ns := getEnv("NAMESPACES", "default")
	cfg.Namespaces = splitTrim(ns, ",")

	// Agent
	cfg.Agent = AgentConfig{
		MaxSteps:        getEnvInt("AGENT_MAX_STEPS", 12),
		StepTimeout:     getEnvDuration("AGENT_STEP_TIMEOUT", 60*time.Second),
		InvestigTimeout: getEnvDuration("AGENT_INVESTIG_TIMEOUT", 5*time.Minute),
		PromptPath:      getEnv("PROMPT_PATH", "/etc/srengine/prompt.txt"),
		SummarizeEvery:  getEnvInt("AGENT_SUMMARIZE_EVERY", 6),
	}

	// LLM
	cfg.LLM = LLMConfig{
		Provider:   getEnv("LLM_PROVIDER", "ollama"),
		OllamaURL:  getEnv("OLLAMA_URL", "http://ollama:11434"),
		Model:      getEnv("OLLAMA_MODEL", "qwen2.5:7b"),
		Timeout:    getEnvDuration("LLM_TIMEOUT", 120*time.Second),
		MaxRetries: getEnvInt("LLM_MAX_RETRIES", 2),
	}

	// Logs
	cfg.Logs = LogsConfig{
		MaxLines:    getEnvInt("LOGS_MAX_LINES", 500),
		MaxPatterns: getEnvInt("LOGS_MAX_PATTERNS", 20),
		Levels:      splitTrim(getEnv("LOGS_LEVELS", "ERROR,WARN"), ","),
	}

	// Server
	cfg.Server = ServerConfig{
		Addr: getEnv("SERVER_ADDR", ":8080"),
	}

	// Metrics
	cfg.Metrics = MetricsConfig{
		PrometheusURL: getEnv("PROMETHEUS_URL", ""),
		Timeout:       getEnvDuration("METRICS_TIMEOUT", 15*time.Second),
	}

	// Notifier
	cfg.Notifier = NotifierConfig{
		Telegram: TelegramConfig{
			Enabled: getEnvBool("TELEGRAM_ENABLED", false),
			Token:   getEnv("TELEGRAM_TOKEN", ""),
			ChatID:  getEnv("TELEGRAM_CHAT_ID", ""),
		},
		Email: EmailConfig{
			Enabled:  getEnvBool("EMAIL_ENABLED", false),
			SMTPHost: getEnv("EMAIL_SMTP_HOST", ""),
			SMTPPort: getEnvInt("EMAIL_SMTP_PORT", 587),
			From:     getEnv("EMAIL_FROM", ""),
			To:       splitTrim(getEnv("EMAIL_TO", ""), ","),
			Password: getEnv("EMAIL_PASSWORD", ""),
		},
		Webhook: WebhookConfig{
			Enabled: getEnvBool("WEBHOOK_ENABLED", false),
			URL:     getEnv("WEBHOOK_URL", ""),
			Timeout: getEnvDuration("WEBHOOK_TIMEOUT", 10*time.Second),
		},
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if len(c.Namespaces) == 0 {
		return fmt.Errorf("NAMESPACES must not be empty")
	}
	if c.LLM.OllamaURL == "" {
		return fmt.Errorf("OLLAMA_URL must not be empty")
	}
	// Notifier
	if c.Notifier.Telegram.Enabled {
		if c.Notifier.Telegram.Token == "" || c.Notifier.Telegram.ChatID == "" {
			return fmt.Errorf("TELEGRAM_TOKEN and TELEGRAM_CHAT_ID required when telegram enabled")
		}
	}
	if c.Notifier.Email.Enabled {
		if c.Notifier.Email.SMTPHost == "" || c.Notifier.Email.From == "" || len(c.Notifier.Email.To) == 0 {
			return fmt.Errorf("EMAIL_SMTP_HOST, EMAIL_FROM, EMAIL_TO required when email enabled")
		}
	}
	if c.Notifier.Webhook.Enabled {
		if c.Notifier.Webhook.URL == "" {
			return fmt.Errorf("WEBHOOK_URL required when webhook enabled")
		}
	}
	return nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func splitTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			result = append(result, t)
		}
	}
	return result
}

func getEnvBool(key string, def bool) bool {
    if v := os.Getenv(key); v != "" {
        if b, err := strconv.ParseBool(v); err == nil {
            return b
        }
    }
    return def
}