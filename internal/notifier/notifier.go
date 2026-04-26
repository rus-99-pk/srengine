package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/your-org/ai-sre/internal/agent"
	"github.com/your-org/ai-sre/internal/config"
)

// New — фабрика нотификаторов
func New(cfg config.NotifierConfig) (agent.Notifier, error) {
	switch cfg.Type {
	case "stdout":
		return &StdoutNotifier{}, nil
	case "telegram":
		return &TelegramNotifier{
			cfg:    cfg.Telegram,
			client: &http.Client{Timeout: 10 * time.Second},
		}, nil
	case "email":
		// TODO: реализовать EmailNotifier
		return nil, fmt.Errorf("email notifier not yet implemented, see TODO.md")
	default:
		return nil, fmt.Errorf("unknown notifier type: %q (supported: stdout, telegram)", cfg.Type)
	}
}

// --- StdoutNotifier ---

type StdoutNotifier struct{}

func (n *StdoutNotifier) Send(ctx context.Context, report *agent.Report) error {
	data, err := json.Marshal(map[string]any{
		"level":              "info",
		"event":             "investigation_complete",
		"alert":             report.AlertName,
		"namespace":         report.Namespace,
		"root_cause":        report.RootCause,
		"summary":           report.Summary,
		"confidence":        report.Confidence,
		"steps_used":        report.StepsUsed,
		"duration":          report.Duration,
		"actions":           report.Actions,
		"skipped_namespaces": report.SkippedNamespaces,
		"parse_error":       report.ParseError,
	})
	if err != nil {
		return err
	}
	slog.Info(string(data))
	return nil
}

// --- TelegramNotifier ---

type TelegramNotifier struct {
	cfg    config.TelegramConfig
	client *http.Client
}

func (n *TelegramNotifier) Send(ctx context.Context, report *agent.Report) error {
	text := formatTelegramMessage(report)

	payload := map[string]any{
		"chat_id":    n.cfg.ChatID,
		"text":       text,
		"parse_mode": "Markdown",
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram returned status %d", resp.StatusCode)
	}

	return nil
}

func formatTelegramMessage(r *agent.Report) string {
	var sb strings.Builder

	// Заголовок
	icon := confidenceIcon(r.Confidence)
	fmt.Fprintf(&sb, "%s *AI SRE Investigation Complete*\n\n", icon)
	fmt.Fprintf(&sb, "🔔 *Alert:* `%s`\n", r.AlertName)
	fmt.Fprintf(&sb, "📦 *Namespace:* `%s`\n", r.Namespace)
	fmt.Fprintf(&sb, "🎯 *Confidence:* %s\n\n", r.Confidence)

	// Root cause
	fmt.Fprintf(&sb, "*Root Cause:*\n%s\n\n", r.RootCause)

	// Summary
	if r.Summary != "" {
		fmt.Fprintf(&sb, "*Summary:*\n%s\n\n", r.Summary)
	}

	// Actions
	if len(r.Actions) > 0 {
		fmt.Fprintf(&sb, "*Recommended Actions:*\n")
		for _, a := range r.Actions {
			riskIcon := riskIcon(a.RiskLevel)
			fmt.Fprintf(&sb, "%s %d. %s\n", riskIcon, a.Priority, a.Description)
			if a.Command != "" {
				fmt.Fprintf(&sb, "   `%s`\n", a.Command)
			}
		}
		fmt.Fprintf(&sb, "\n")
	}

	// Skipped namespaces
	if len(r.SkippedNamespaces) > 0 {
		fmt.Fprintf(&sb, "⚠️ *Skipped namespaces:* %s\n", strings.Join(r.SkippedNamespaces, ", "))
	}

	// Footer
	fmt.Fprintf(&sb, "\n_Steps: %d | Duration: %s_", r.StepsUsed, r.Duration)

	return sb.String()
}

func confidenceIcon(c string) string {
	switch c {
	case "high":
		return "✅"
	case "medium":
		return "⚠️"
	default:
		return "❓"
	}
}

func riskIcon(r string) string {
	switch r {
	case "high":
		return "🔴"
	case "medium":
		return "🟡"
	default:
		return "🟢"
	}
}
