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
	Type     string // stdout | telegram | email
	Telegram TelegramConfig
	Email    EmailConfig
}

type TelegramConfig struct {
	Token  string
	ChatID string
}

type EmailConfig struct {
	SMTPHost string
	SMTPPort int
	From     string
	To       string
	Password string
}

func Load() (*Config, error) {
	cfg := &Config{}

	// Namespaces — через запятую
	ns := getEnv("NAMESPACES", "default")
	cfg.Namespaces = splitTrim(ns, ",")

	// Agent
	cfg.Agent = AgentConfig{
		MaxSteps:        getEnvInt("AGENT_MAX_STEPS", 8),
		StepTimeout:     getEnvDuration("AGENT_STEP_TIMEOUT", 60*time.Second),
		InvestigTimeout: getEnvDuration("AGENT_INVESTIG_TIMEOUT", 5*time.Minute),
		PromptPath:      getEnv("PROMPT_PATH", "/etc/ai-sre/prompt.txt"),
		SummarizeEvery:  getEnvInt("AGENT_SUMMARIZE_EVERY", 3),
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

	// Notifier
	cfg.Notifier = NotifierConfig{
		Type: getEnv("NOTIFIER_TYPE", "stdout"),
		Telegram: TelegramConfig{
			Token:  getEnv("TELEGRAM_TOKEN", ""),
			ChatID: getEnv("TELEGRAM_CHAT_ID", ""),
		},
		Email: EmailConfig{
			SMTPHost: getEnv("EMAIL_SMTP_HOST", ""),
			SMTPPort: getEnvInt("EMAIL_SMTP_PORT", 587),
			From:     getEnv("EMAIL_FROM", ""),
			To:       getEnv("EMAIL_TO", ""),
			Password: getEnv("EMAIL_PASSWORD", ""),
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
	if c.Notifier.Type == "telegram" {
		if c.Notifier.Telegram.Token == "" || c.Notifier.Telegram.ChatID == "" {
			return fmt.Errorf("TELEGRAM_TOKEN and TELEGRAM_CHAT_ID required")
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