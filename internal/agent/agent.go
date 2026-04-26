package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/your-org/ai-sre/internal/alert"
	"github.com/your-org/ai-sre/internal/config"
)

// --- Types ---

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ToolCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type ThoughtAction struct {
	Thought string      `json:"thought"`
	Action  *ToolCall   `json:"action,omitempty"`
	Answer  *Report     `json:"answer,omitempty"`
}

type Action struct {
	Priority    int    `json:"priority"`
	Description string `json:"description"`
	Command     string `json:"command,omitempty"`
	RiskLevel   string `json:"risk_level"` // low | medium | high
}

type Report struct {
	Summary            string    `json:"summary"`
	RootCause          string    `json:"root_cause"`
	Confidence         string    `json:"confidence"` // high | medium | low
	Actions            []Action  `json:"actions"`
	SkippedNamespaces  []string  `json:"skipped_namespaces,omitempty"`
	StepsUsed          int       `json:"steps_used"`
	Duration           string    `json:"duration"`
	AlertName          string    `json:"alert_name"`
	Namespace          string    `json:"namespace"`
	ParseError         bool      `json:"parse_error,omitempty"`
	RawResponse        string    `json:"raw_response,omitempty"`
}

// --- Interfaces ---

type LLMProvider interface {
	Complete(ctx context.Context, messages []Message) (string, error)
	Name() string
}

type ToolRegistry interface {
	Execute(ctx context.Context, call *ToolCall) (string, error)
	Descriptions() string // список инструментов для system prompt
}

type Notifier interface {
	Send(ctx context.Context, report *Report) error
}

// --- Agent ---

type Deps struct {
	LLM      LLMProvider
	Tools    ToolRegistry
	Notifier Notifier
	Logger   *slog.Logger
	Config   config.AgentConfig
}

type Agent struct {
	deps Deps
}

func New(deps Deps) *Agent {
	return &Agent{deps: deps}
}

// Investigate — главная точка входа. Реализует Investigator интерфейс.
func (a *Agent) Investigate(ctx context.Context, al *alert.Alert) error {
	start := time.Now()
	log := a.deps.Logger.With("alert", al.Name, "namespace", al.Namespace)
	log.Info("starting investigation")

	ctx, cancel := context.WithTimeout(ctx, a.deps.Config.InvestigTimeout)
	defer cancel()

	report, err := a.runReAct(ctx, al)
	if err != nil {
		log.Error("react loop failed", "err", err)
		// Возвращаем частичный отчёт даже при ошибке
		report = &Report{
			AlertName:  al.Name,
			Namespace:  al.Namespace,
			Summary:    "Investigation failed: " + err.Error(),
			Confidence: "low",
			ParseError: true,
		}
	}

	report.Duration = time.Since(start).Round(time.Second).String()
	report.AlertName = al.Name
	report.Namespace = al.Namespace

	log.Info("investigation complete",
		"root_cause", report.RootCause,
		"confidence", report.Confidence,
		"steps", report.StepsUsed,
		"duration", report.Duration,
	)

	// Отправляем уведомление
	if err := a.deps.Notifier.Send(ctx, report); err != nil {
		log.Error("failed to send notification", "err", err)
	}

	return nil
}

// runReAct — ReAct loop: Think → Act → Observe → repeat
func (a *Agent) runReAct(ctx context.Context, al *alert.Alert) (*Report, error) {
	systemPrompt, err := a.loadPrompt(al)
	if err != nil {
		return nil, fmt.Errorf("load prompt: %w", err)
	}

	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: a.buildAlertContext(al)},
	}

	seenCalls := make(map[string]struct{})

	for step := 0; step < a.deps.Config.MaxSteps; step++ {
		a.deps.Logger.Info("react step", "step", step+1)

		// Think: спрашиваем модель
		raw, err := a.completeWithRetry(ctx, messages)
		if err != nil {
			return nil, fmt.Errorf("llm complete step %d: %w", step, err)
		}

		// Парсим ответ
		ta, parseErr := parseResponse(raw)
		if parseErr != nil {
			a.deps.Logger.Error("failed to parse model response",
				"step", step+1,
				"raw", raw,
				"err", parseErr,
			)
			// Частичный отчёт если не смогли распарсить после retry
			return &Report{
				Summary:     "Failed to parse model response",
				Confidence:  "low",
				StepsUsed:   step + 1,
				ParseError:  true,
				RawResponse: raw,
			}, nil
		}

		// Добавляем ответ модели в историю
		messages = append(messages, Message{Role: "assistant", Content: raw})

		// Answer: модель решила что данных достаточно
		if ta.Answer != nil {
			ta.Answer.StepsUsed = step + 1
			return ta.Answer, nil
		}

		// Act: выполняем tool call
		if ta.Action == nil {
			return nil, fmt.Errorf("step %d: no action and no answer", step)
		}

		// Защита от повторных вызовов с теми же аргументами
		callKey := fmt.Sprintf("%s:%v", ta.Action.Tool, ta.Action.Args)
		if _, seen := seenCalls[callKey]; seen {
			a.deps.Logger.Warn("duplicate tool call skipped",
				"tool", ta.Action.Tool,
				"args", ta.Action.Args,
			)
			messages = append(messages, Message{
				Role:    "user",
				Content: `{"tool":"` + ta.Action.Tool + `","result":"skipped: already called with same arguments, try a different tool or conclude"}`,
			})
			continue
		}
		seenCalls[callKey] = struct{}{}

		result, toolErr := a.deps.Tools.Execute(ctx, ta.Action)
		if toolErr != nil {
			result = fmt.Sprintf("error: %s", toolErr.Error())
		}

		a.deps.Logger.Info("tool executed",
			"tool", ta.Action.Tool,
			"thought", ta.Thought,
		)

		// Observe: добавляем результат в контекст
		toolResult, _ := json.Marshal(map[string]string{
			"tool":   ta.Action.Tool,
			"result": result,
		})
		messages = append(messages, Message{
			Role:    "user",
			Content: string(toolResult),
		})

		// Context summarization: каждые N шагов сжимаем историю
		if a.deps.Config.SummarizeEvery > 0 &&
			(step+1)%a.deps.Config.SummarizeEvery == 0 {
			messages = a.summarizeContext(ctx, messages)
		}
	}

	// Исчерпали шаги — просим финальный ответ
	messages = append(messages, Message{
		Role:    "user",
		Content: "Max steps reached. Provide your best answer now based on collected data.",
	})

	raw, err := a.completeWithRetry(ctx, messages)
	if err != nil {
		return nil, err
	}

	ta, _ := parseResponse(raw)
	if ta != nil && ta.Answer != nil {
		ta.Answer.StepsUsed = a.deps.Config.MaxSteps
		return ta.Answer, nil
	}

	return &Report{
		Summary:     "Max steps reached, could not determine root cause",
		Confidence:  "low",
		StepsUsed:   a.deps.Config.MaxSteps,
		RawResponse: raw,
	}, nil
}

// summarizeContext — сжимает накопленную историю ReAct в короткое резюме.
// Сохраняет system prompt и alert context, заменяет все ReAct turns одним summary.
func (a *Agent) summarizeContext(ctx context.Context, messages []Message) []Message {
	// Первые два сообщения не трогаем: system + alert context
	if len(messages) <= 2 {
		return messages
	}

	fixed := messages[:2]
	history := messages[2:]

	// Собираем историю в текст для сжатия
	var sb strings.Builder
	sb.WriteString("Summarize the investigation so far in 3-5 sentences. Include: what resources were checked, what errors were found, what services are involved, current hypothesis. Be concise.\n\nHistory:\n")
	for _, m := range history {
		fmt.Fprintf(&sb, "[%s]: %s\n", m.Role, m.Content)
	}

	summaryMessages := []Message{
		{Role: "system", Content: "You are a summarization assistant. Respond with plain text summary only, no JSON."},
		{Role: "user", Content: sb.String()},
	}

	summary, err := a.deps.LLM.Complete(ctx, summaryMessages)
	if err != nil {
		// Если не смогли сжать — возвращаем оригинал
		a.deps.Logger.Warn("context summarization failed, keeping original", "err", err)
		return messages
	}

	a.deps.Logger.Info("context summarized",
		"original_messages", len(history),
		"summary_len", len(summary),
	)

	// Заменяем всю историю одним summary сообщением
	return append(fixed, Message{
		Role:    "user",
		Content: "Investigation summary so far: " + summary + "\nContinue the investigation.",
	})
}

// completeWithRetry — retry если модель вернула невалидный JSON (max 2 попытки)
func (a *Agent) completeWithRetry(ctx context.Context, messages []Message) (string, error) {
	const maxRetries = 2
	var lastRaw string
	for attempt := 0; attempt <= maxRetries; attempt++ {
		msgs := messages
		if attempt > 0 {
			// Напоминаем про формат
			msgs = append(msgs, Message{
				Role:    "user",
				Content: `Respond ONLY with valid JSON. No markdown, no explanation. Just JSON.`,
			})
		}

		raw, err := a.deps.LLM.Complete(ctx, msgs)
		if err != nil {
			return "", err
		}

		lastRaw = raw
		if _, err := parseResponse(raw); err == nil {
			return raw, nil
		}

		if attempt >= maxRetries {
			break
		}
	}
	return lastRaw, nil // вернём сырой, caller обработает
}

// loadPrompt — читает system prompt из файла (перечитывается каждый раз)
func (a *Agent) loadPrompt(al *alert.Alert) (string, error) {
	data, err := os.ReadFile(a.deps.Config.PromptPath)
	if err != nil {
		return "", fmt.Errorf("read prompt file %s: %w", a.deps.Config.PromptPath, err)
	}

	// Подставляем переменные
	prompt := string(data)
	prompt = strings.ReplaceAll(prompt, "{{TOOLS}}", a.deps.Tools.Descriptions())
	return prompt, nil
}

// buildAlertContext — первое user-сообщение с данными алерта
func (a *Agent) buildAlertContext(al *alert.Alert) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "ALERT: %s\n", al.Name)
	fmt.Fprintf(&sb, "Status: %s\n", al.Status)
	fmt.Fprintf(&sb, "Namespace: %s\n", al.Namespace)
	fmt.Fprintf(&sb, "Started: %s\n", al.StartsAt.Format(time.RFC3339))

	if len(al.Labels) > 0 {
		fmt.Fprintf(&sb, "Labels:\n")
		for k, v := range al.Labels {
			fmt.Fprintf(&sb, "  %s: %s\n", k, v)
		}
	}

	if u := al.RunbookURL(); u != "" {
		fmt.Fprintf(&sb, "Runbook: %s\n", u)
	}

	fmt.Fprintf(&sb, "\nInvestigate this alert. Start with the most affected resource.")
	return sb.String()
}

// parseResponse — парсит JSON ответ модели
func parseResponse(raw string) (*ThoughtAction, error) {
	// Убираем возможные markdown обёртки
	clean := strings.TrimSpace(raw)
	clean = strings.TrimPrefix(clean, "```json")
	clean = strings.TrimPrefix(clean, "```")
	clean = strings.TrimSuffix(clean, "```")
	clean = strings.TrimSpace(clean)

	// Извлекаем первый валидный JSON объект — обрезаем trailing мусор
	// Модели иногда добавляют лишние }} или пробелы после JSON
	clean = extractFirstJSON(clean)

	var ta ThoughtAction
	if err := json.Unmarshal([]byte(clean), &ta); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}

	if ta.Thought == "" {
		return nil, fmt.Errorf("empty thought field")
	}

	if ta.Action == nil && ta.Answer == nil {
		return nil, fmt.Errorf("neither action nor answer present")
	}

	return &ta, nil
}

// extractFirstJSON — находит первый балансированный JSON объект в строке
func extractFirstJSON(s string) string {
	start := strings.Index(s, "{")
	if start == -1 {
		return s
	}

	depth := 0
	inStr := false
	escaped := false

	for i := start; i < len(s); i++ {
		c := s[i]

		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inStr {
			escaped = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}

		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}

	return s
}