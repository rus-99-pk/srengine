package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"github.com/rus-99-pk/srengine/internal/agent"
	"github.com/rus-99-pk/srengine/internal/config"
)

// Notifier defines the common interface for sending investigation reports.
type Notifier interface {
	Send(ctx context.Context, report *agent.Report) error
	Name() string
}

// New builds a MultiNotifier from all enabled channels in the config.
func New(cfg config.NotifierConfig) (Notifier, error) {
	var notifiers []Notifier

	if cfg.Telegram.Enabled {
		notifiers = append(notifiers, newTelegramNotifier(cfg.Telegram))
	}

	if cfg.Email.Enabled {
		n, err := newEmailNotifier(cfg.Email)
		if err != nil {
			return nil, fmt.Errorf("email notifier: %w", err)
		}
		notifiers = append(notifiers, n)
	}

	if cfg.Webhook.Enabled {
		notifiers = append(notifiers, newWebhookNotifier(cfg.Webhook))
	}

	// Fallback to stdout if no specific channels are configured.
	if len(notifiers) == 0 {
		slog.Warn("no notifiers configured, falling back to stdout")
		return &StdoutNotifier{}, nil
	}

	return &MultiNotifier{notifiers: notifiers}, nil
}

// MultiNotifier broadcasts the report to all configured channels concurrently.
type MultiNotifier struct {
	notifiers []Notifier
}

func (m *MultiNotifier) Name() string {
	names := make([]string, len(m.notifiers))
	for i, n := range m.notifiers {
		names[i] = n.Name()
	}
	return "multi[" + strings.Join(names, ",") + "]"
}

// Send executes notifications concurrently and aggregates errors (Fanout pattern).
func (m *MultiNotifier) Send(ctx context.Context, report *agent.Report) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []string
	)

	for _, n := range m.notifiers {
		n := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := n.Send(ctx, report); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("%s: %v", n.Name(), err))
				mu.Unlock()
				slog.Error("notifier failed", "notifier", n.Name(), "err", err)
			} else {
				slog.Info("notification sent", "notifier", n.Name())
			}
		}()
	}

	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("some notifiers failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

// StdoutNotifier prints the investigation report as a structured JSON log.
type StdoutNotifier struct{}

func (n *StdoutNotifier) Name() string { return "stdout" }

func (n *StdoutNotifier) Send(_ context.Context, report *agent.Report) error {
	data, err := json.Marshal(map[string]any{
		"level":              "info",
		"event":              "investigation_complete",
		"alert":              report.AlertName,
		"namespace":          report.Namespace,
		"root_cause":         report.RootCause,
		"summary":            report.Summary,
		"confidence":         report.Confidence,
		"steps_used":         report.StepsUsed,
		"duration":           report.Duration,
		"actions":            report.Actions,
		"skipped_namespaces": report.SkippedNamespaces,
		"parse_error":        report.ParseError,
	})
	if err != nil {
		return err
	}
	slog.Info(string(data))
	return nil
}

// TelegramNotifier sends the formatted report to a Telegram chat.
type TelegramNotifier struct {
	cfg    config.TelegramConfig
	client *http.Client
}

func newTelegramNotifier(cfg config.TelegramConfig) *TelegramNotifier {
	return &TelegramNotifier{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (n *TelegramNotifier) Name() string { return "telegram" }

func (n *TelegramNotifier) Send(ctx context.Context, report *agent.Report) error {
	text := formatTelegramMessage(report)

	payload, err := json.Marshal(map[string]any{
		"chat_id":    n.cfg.ChatID,
		"text":       text,
		"parse_mode": "Markdown",
	})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// formatTelegramMessage constructs a markdown-formatted message string for Telegram.
func formatTelegramMessage(r *agent.Report) string {
	var sb strings.Builder

	icon := confidenceIcon(r.Confidence)
	fmt.Fprintf(&sb, "%s *AI SRE Investigation Complete*\n\n", icon)
	fmt.Fprintf(&sb, "🔔 *Alert:* `%s`\n", r.AlertName)
	if r.Namespace != "" {
		fmt.Fprintf(&sb, "📦 *Namespace:* `%s`\n", r.Namespace)
	}
	fmt.Fprintf(&sb, "🎯 *Confidence:* %s\n\n", r.Confidence)

	fmt.Fprintf(&sb, "*Root Cause:*\n%s\n\n", r.RootCause)

	if r.Summary != "" {
		fmt.Fprintf(&sb, "*Summary:*\n%s\n\n", r.Summary)
	}

	if len(r.Actions) > 0 {
		fmt.Fprintf(&sb, "*Recommended Actions:*\n")
		for _, a := range r.Actions {
			fmt.Fprintf(&sb, "%s %d. %s\n", riskIcon(a.RiskLevel), a.Priority, a.Description)
			if a.Command != "" {
				fmt.Fprintf(&sb, "   `%s`\n", a.Command)
			}
		}
		fmt.Fprintf(&sb, "\n")
	}

	if len(r.SkippedNamespaces) > 0 {
		fmt.Fprintf(&sb, "⚠️ *Skipped namespaces:* %s\n", strings.Join(r.SkippedNamespaces, ", "))
	}

	fmt.Fprintf(&sb, "\n_Steps: %d | Duration: %s_", r.StepsUsed, r.Duration)
	return sb.String()
}

// EmailNotifier sends HTML-formatted reports via SMTP.
type EmailNotifier struct {
	cfg      config.EmailConfig
	template *template.Template
}

func newEmailNotifier(cfg config.EmailConfig) (*EmailNotifier, error) {
	funcMap := template.FuncMap{
		"join": strings.Join,
	}
	tmpl, err := template.New("email").Funcs(funcMap).Parse(emailHTMLTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse email template: %w", err)
	}
	return &EmailNotifier{cfg: cfg, template: tmpl}, nil
}

func (n *EmailNotifier) Name() string { return "email" }

func (n *EmailNotifier) Send(_ context.Context, report *agent.Report) error {
	body, err := n.renderHTML(report)
	if err != nil {
		return fmt.Errorf("render template: %w", err)
	}

	subject := fmt.Sprintf("[SREngine] %s — %s (%s)",
		report.AlertName, report.RootCause, report.Confidence)
	if len(subject) > 120 {
		subject = subject[:117] + "..."
	}

	msg := buildMIMEMessage(n.cfg.From, n.cfg.To, subject, body)

	addr := fmt.Sprintf("%s:%d", n.cfg.SMTPHost, n.cfg.SMTPPort)

	// Use anonymous relay if no password is provided.
	var auth smtp.Auth
	if n.cfg.Password != "" {
		auth = smtp.PlainAuth("", n.cfg.From, n.cfg.Password, n.cfg.SMTPHost)
	}

	if err := smtp.SendMail(addr, auth, n.cfg.From, n.cfg.To, msg); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

// renderHTML executes the email template using the report data.
func (n *EmailNotifier) renderHTML(r *agent.Report) (string, error) {
	var buf bytes.Buffer
	if err := n.template.Execute(&buf, r); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// buildMIMEMessage constructs the raw MIME bytes for the HTML email.
func buildMIMEMessage(from string, to []string, subject, htmlBody string) []byte {
	var sb strings.Builder
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	fmt.Fprintf(&sb, "From: SREngine <%s>\r\n", from)
	fmt.Fprintf(&sb, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&sb, "Subject: %s\r\n", subject)
	sb.WriteString("\r\n")
	sb.WriteString(htmlBody)
	return []byte(sb.String())
}

// WebhookNotifier sends the report as a JSON payload via HTTP POST.
type WebhookNotifier struct {
	cfg    config.WebhookConfig
	client *http.Client
}

func newWebhookNotifier(cfg config.WebhookConfig) *WebhookNotifier {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &WebhookNotifier{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

func (n *WebhookNotifier) Name() string { return "webhook" }

func (n *WebhookNotifier) Send(ctx context.Context, report *agent.Report) error {
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "srengine/1.0")

	// Apply custom HTTP headers from configuration.
	for k, v := range n.cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// confidenceIcon returns an emoji representing the confidence level.
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

// riskIcon returns an emoji representing the risk level.
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

// emailHTMLTemplate is the embedded HTML template for the email notification.
const emailHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
         background:#f5f5f5; margin:0; padding:24px; color:#1a1a1a; }
  .card { background:#fff; border-radius:8px; max-width:680px; margin:0 auto;
          padding:32px; box-shadow:0 1px 4px rgba(0,0,0,.08); }
  h1 { font-size:20px; margin:0 0 24px; }
  .badge { display:inline-block; padding:2px 10px; border-radius:12px;
           font-size:13px; font-weight:600; }
  .high   { background:#d1fae5; color:#065f46; }
  .medium { background:#fef3c7; color:#92400e; }
  .low    { background:#fee2e2; color:#991b1b; }
  table { width:100%; border-collapse:collapse; margin:16px 0; }
  td { padding:8px 12px; border-bottom:1px solid #f0f0f0; font-size:14px; }
  td:first-child { color:#6b7280; width:160px; white-space:nowrap; }
  .section { margin:24px 0 8px; font-size:13px; font-weight:600;
             text-transform:uppercase; letter-spacing:.05em; color:#6b7280; }
  .root-cause { background:#f8fafc; border-left:3px solid #6366f1;
                padding:12px 16px; border-radius:0 6px 6px 0;
                font-size:15px; line-height:1.5; }
  .action { padding:10px 0; border-bottom:1px solid #f0f0f0; }
  .action:last-child { border-bottom:none; }
  .action-title { font-size:14px; }
  .cmd { font-family:monospace; background:#f1f5f9; padding:4px 8px;
         border-radius:4px; font-size:13px; margin-top:4px; display:block; }
  .risk { font-size:12px; margin-left:6px; }
  .risk-low    { color:#059669; }
  .risk-medium { color:#d97706; }
  .risk-high   { color:#dc2626; }
  .footer { margin-top:32px; font-size:12px; color:#9ca3af; text-align:center; }
  .skipped { background:#fff7ed; border:1px solid #fed7aa;
             padding:8px 12px; border-radius:6px; font-size:13px; }
</style>
</head>
<body>
<div class="card">
  <h1>🤖 SREngine Investigation Report</h1>

  <table>
    <tr><td>Alert</td>     <td><strong>{{.AlertName}}</strong></td></tr>
    {{if .Namespace}}
    <tr><td>Namespace</td> <td><code>{{.Namespace}}</code></td></tr>
    {{end}}
    <tr><td>Confidence</td>
        <td><span class="badge {{.Confidence}}">{{.Confidence}}</span></td></tr>
    <tr><td>Steps used</td><td>{{.StepsUsed}}</td></tr>
    <tr><td>Duration</td>  <td>{{.Duration}}</td></tr>
  </table>

  <div class="section">Root Cause</div>
  <div class="root-cause">{{.RootCause}}</div>

  {{if .Summary}}
  <div class="section">Summary</div>
  <p style="font-size:14px;line-height:1.6;margin:0">{{.Summary}}</p>
  {{end}}

  {{if .Actions}}
  <div class="section">Recommended Actions</div>
  {{range .Actions}}
  <div class="action">
    <div class="action-title">
      <strong>{{.Priority}}.</strong> {{.Description}}
      <span class="risk risk-{{.RiskLevel}}">[{{.RiskLevel}} risk]</span>
    </div>
    {{if .Command}}<code class="cmd">{{.Command}}</code>{{end}}
  </div>
  {{end}}
  {{end}}

  {{if .SkippedNamespaces}}
  <div class="section">Skipped Namespaces</div>
  <div class="skipped">⚠️ {{join .SkippedNamespaces ", "}}</div>
  {{end}}

  <div class="footer">
    Generated by SREngine &middot; {{.Duration}}
  </div>
</div>
</body>
</html>`