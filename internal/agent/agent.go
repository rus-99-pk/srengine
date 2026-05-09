package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/rus-99-pk/srengine/internal/alert"
	"github.com/rus-99-pk/srengine/internal/config"
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

// Investigate — главная точка входа. Возвращает (any, error) для совместимости с сервером.
func (a *Agent) Investigate(ctx context.Context, al *alert.Alert) (any, error) {
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

	return report, nil // <-- ТЕПЕРЬ ВОЗВРАЩАЕМ ОТЧЕТ СЕРВЕРУ
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
	consecutiveDupes := 0

	for step := 0; step < a.deps.Config.MaxSteps; step++ {
		a.deps.Logger.Info("react step", "step", step+1)

		stepsLeft := a.deps.Config.MaxSteps - step
		if stepsLeft == 2 {
			// Предупреждаем модель что шаги заканчиваются
			messages = append(messages, Message{
				Role:    "user",
				Content: "WARNING: only 2 steps remaining. If you have enough data — provide your answer now. Otherwise make one final tool call.",
			})
		}

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
			consecutiveDupes++
			a.deps.Logger.Warn("duplicate tool call skipped",
				"tool", ta.Action.Tool,
				"args", ta.Action.Args,
				"consecutive", consecutiveDupes,
			)

			// После 1 подряд дубля — принудительно запрашиваем финальный ответ
			if consecutiveDupes >= 1 {
				forceMsg := `You are repeating tool calls you already made. STOP. You must now provide your FINAL answer based on everything collected so far. Respond with answer JSON only:
{"thought":"<your reasoning>","answer":{"summary":"...","root_cause":"...","confidence":"low|medium|high","actions":[...],"skipped_namespaces":[]}}`
				messages = append(messages, Message{Role: "user", Content: forceMsg})
				raw, err := a.completeWithRetry(ctx, messages)
				if err != nil {
					break
				}
				ta, _ := parseResponse(raw)
				if ta != nil && ta.Answer != nil {
					ta.Answer.StepsUsed = step + 1
					return ta.Answer, nil
				}
				// Если модель всё равно не дала answer — выходим
				break
			}

			messages = append(messages, Message{
				Role:    "user",
				Content: `{"tool":"` + ta.Action.Tool + `","result":"skipped: already called with same arguments, try a different tool or conclude"}`,
			})
			continue
		}
		consecutiveDupes = 0
		seenCalls[callKey] = struct{}{}

		result, toolErr := a.deps.Tools.Execute(ctx, ta.Action)
		if toolErr != nil {
			result = fmt.Sprintf("error: %s", toolErr.Error())
		}

		a.deps.Logger.Info("tool executed",
			"tool", ta.Action.Tool,
			"args", ta.Action.Args,
			"thought", ta.Thought,
		)

		a.deps.Logger.Info("tool result",
			"tool", ta.Action.Tool,
			"result", func() string {
				if len(result) > 300 {
					return result[:300] + "..."
				}
				return result
			}(),
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

		// Интерпретатор check_metrics: если вернулась утилизация — помогаем модели сделать вывод
		if ta.Action.Tool == "check_metrics" {
			if hint := interpretMemoryMetrics(result); hint != "" {
				messages = append(messages, Message{Role: "user", Content: hint})
			}
		}

		// Детектор quota — принудительно завершаем если events показали quota exceeded
		if ta.Action.Tool == "get_events" &&
			(strings.Contains(result, "exceeded quota") ||
				strings.Contains(result, "FailedCreate") ||
				strings.Contains(result, "forbidden")) {
			messages = append(messages, Message{
				Role: "user",
				Content: `QUOTA EXCEEDED detected in events. This IS the root cause with confidence=high.
		Provide your FINAL answer NOW. No more tool calls needed. Use this format:
		{"thought":"quota exceeded is the root cause","answer":{"summary":"...","root_cause":"...","confidence":"high","actions":[...],"skipped_namespaces":[]}}`,
			})
			raw, err := a.completeWithRetry(ctx, messages)
			if err == nil {
				if parsed, _ := parseResponse(raw); parsed != nil && parsed.Answer != nil {
					parsed.Answer.StepsUsed = step + 1
					return parsed.Answer, nil
				}
			}
			// Если не смогли распарсить — продолжаем обычный цикл
		}

		// Детектор NotReady ноды
		if ta.Action.Tool == "describe_resource" &&
			strings.Contains(result, "Kubelet stopped posting node status") {
			messages = append(messages, Message{
				Role: "user",
				Content: `NODE NOT READY detected: kubelet stopped posting node status.
		This IS the root cause with confidence=high. Provide your FINAL answer NOW:
		{"thought":"kubelet stopped posting node status","answer":{"summary":"...","root_cause":"Node lost heartbeat: kubelet stopped posting node status","confidence":"high","actions":[{"priority":1,"description":"Check node connectivity and restart kubelet","command":"kubectl describe node <node>","risk_level":"medium"}],"skipped_namespaces":[]}}`,
			})
			raw, err := a.completeWithRetry(ctx, messages)
			if err == nil {
				if parsed, _ := parseResponse(raw); parsed != nil && parsed.Answer != nil {
					parsed.Answer.StepsUsed = step + 1
					return parsed.Answer, nil
				}
			}
		}

		// Детектор liveness probe с явно сломанной командой
		if ta.Action.Tool == "describe_resource" &&
			strings.Contains(result, "livenessProbe") &&
			(strings.Contains(result, "exit 1") ||
				strings.Contains(result, "exit 2")) {
			messages = append(messages, Message{
				Role: "user",
				Content: `MISCONFIGURED LIVENESS PROBE detected: probe command always returns non-zero exit code.
		This IS the root cause with confidence=high. Provide your FINAL answer NOW:
		{"thought":"liveness probe always fails","answer":{"summary":"...","root_cause":"Liveness probe misconfigured: command always fails","confidence":"high","actions":[{"priority":1,"description":"Fix liveness probe command in deployment","command":"kubectl edit deployment -n <namespace>","risk_level":"low"}],"skipped_namespaces":[]}}`,
			})
			raw, err := a.completeWithRetry(ctx, messages)
			if err == nil {
				if parsed, _ := parseResponse(raw); parsed != nil && parsed.Answer != nil {
					parsed.Answer.StepsUsed = step + 1
					return parsed.Answer, nil
				}
			}
		}

		// Детектор PV filling up — если get_logs вернул disk full
		if ta.Action.Tool == "get_logs" &&
			(strings.Contains(result, "disk full") ||
				strings.Contains(result, "disk usage critical") ||
				strings.Contains(result, "nearly full") ||
				strings.Contains(result, "writes failing")) {
			messages = append(messages, Message{
				Role: "user",
				Content: `DISK FULL detected in pod logs. The PersistentVolume is nearly full.
		This IS the root cause with confidence=high. Provide your FINAL answer NOW:
		{"thought":"disk full detected in logs","answer":{"summary":"...","root_cause":"PersistentVolume is nearly full, application cannot write data","confidence":"high","actions":[{"priority":1,"description":"Expand PVC capacity or clean up disk space","command":"kubectl get pvc -n <namespace>","risk_level":"medium"}],"skipped_namespaces":[]}}`,
			})
			raw, err := a.completeWithRetry(ctx, messages)
			if err == nil {
				if parsed, _ := parseResponse(raw); parsed != nil && parsed.Answer != nil {
					parsed.Answer.StepsUsed = step + 1
					return parsed.Answer, nil
				}
			}
		}

		// Детектор PVC MountedBy — форсируем get_logs для пода который монтирует PVC
		if ta.Action.Tool == "describe_resource" &&
			strings.Contains(result, "MountedBy:") {
			// Извлекаем имя пода из строки "  - pod-name (phase=Running..."
			mountedPod := ""
			for _, line := range strings.Split(result, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "- ") {
					// Формат: "- disk-filler-xxx (phase=Running, restarts=0)"
					part := strings.TrimPrefix(line, "- ")
					if idx := strings.Index(part, " ("); idx > 0 {
						mountedPod = part[:idx]
					}
				}
			}
			if mountedPod != "" {
				// Извлекаем namespace из результата
				ns := ""
				for _, line := range strings.Split(result, "\n") {
					if strings.HasPrefix(line, "PVC: ") {
						// Формат: "PVC: test-pv/small-pvc"
						parts := strings.Split(strings.TrimPrefix(line, "PVC: "), "/")
						if len(parts) == 2 {
							ns = parts[0]
						}
					}
				}
				if ns == "" {
					ns = al.Namespace
				}
				messages = append(messages, Message{
					Role: "user",
					Content: fmt.Sprintf(
						`PVC is mounted by pod "%s". Get its logs NOW to check for disk full errors:
		{"thought":"checking logs of pod mounting the PVC","action":{"tool":"get_logs","args":{"name":"%s","namespace":"%s"}}}`,
						mountedPod, mountedPod, ns),
				})
				raw, err := a.completeWithRetry(ctx, messages)
				if err == nil {
					if parsed, _ := parseResponse(raw); parsed != nil {
						if parsed.Answer != nil {
							parsed.Answer.StepsUsed = step + 1
							return parsed.Answer, nil
						}
						if parsed.Action != nil {
							// Модель согласилась вызвать get_logs — продолжаем цикл
							messages = append(messages, Message{Role: "assistant", Content: raw})
						}
					}
				}
			}
		}

		// Детектор OOMKilled: нет логов (ожидаемо) → сразу финальный ответ
		if ta.Action.Tool == "get_logs" &&
			strings.TrimSpace(result) == "No ERROR/WARN log patterns found" &&
			strings.Contains(al.Name, "CrashLoop") {
			// Проверяем что в предыдущем describe был OOMKilled
			oomDetected := false
			for _, m := range messages {
				if strings.Contains(m.Content, "OOMKilled") {
					oomDetected = true
					break
				}
			}
			if oomDetected {
				messages = append(messages, Message{
					Role: "user",
					Content: `OOMKilled confirmed + empty logs is expected (OOMKilled pods produce no logs).
This IS the root cause with confidence=high. Provide your FINAL answer NOW:
{"thought":"OOMKilled confirmed, empty logs expected","answer":{"summary":"...","root_cause":"Container exceeded its memory limit and was OOMKilled","confidence":"high","actions":[{"priority":1,"description":"Increase memory limit for the container","command":"kubectl edit deployment -n <namespace>","risk_level":"low"}],"skipped_namespaces":[]}}`,
				})
				raw, err := a.completeWithRetry(ctx, messages)
				if err == nil {
					if parsed, _ := parseResponse(raw); parsed != nil && parsed.Answer != nil {
						parsed.Answer.StepsUsed = step + 1
						return parsed.Answer, nil
					}
				}
			}
		}

		// Детектор PodHighMemoryUsage: pod Running + нет логов → форсируем check_metrics
		if ta.Action.Tool == "get_logs" &&
			strings.TrimSpace(result) == "No ERROR/WARN log patterns found" &&
			(strings.Contains(al.Name, "HighMemory") ||
				strings.Contains(al.Name, "MemoryUsage") ||
				strings.Contains(al.Name, "MemoryPressure")) {

			podName := al.Labels["pod"]
			if podName == "" {
				podName, _ = ta.Action.Args["name"].(string)
			}
			ns := al.Namespace

			messages = append(messages, Message{
				Role: "user",
				Content: fmt.Sprintf(
					`No error logs found — expected for PodHighMemoryUsage (pod is Running, not crashing).`+
						` Memory issue is invisible in logs. You MUST call check_metrics to confirm:`+"\n"+
						`{"thought":"no error logs expected for memory alert, checking prometheus metrics",`+
						`"action":{"tool":"check_metrics","args":{`+
						`"promql":"container_memory_working_set_bytes{pod=\"%s\",namespace=\"%s\"}",`+
						`"limit_promql":"kube_pod_container_resource_limits{pod=\"%s\",namespace=\"%s\",resource=\"memory\"}",`+
						`"range_minutes":15}}}`,
					podName, ns, podName, ns),
			})
		}

		// Context summarization
		if a.deps.Config.SummarizeEvery > 0 &&
			(step+1)%a.deps.Config.SummarizeEvery == 0 {
			messages = a.summarizeContext(ctx, messages, seenCalls)
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
func (a *Agent) summarizeContext(ctx context.Context, messages []Message, seenCalls map[string]struct{}) []Message {
	// Первые два сообщения не трогаем: system + alert context
	if len(messages) <= 2 {
		return messages
	}

	fixed := messages[:2]
	history := messages[2:]

	// Собираем список уже вызванных инструментов
	var calledTools strings.Builder
	for key := range seenCalls {
		fmt.Fprintf(&calledTools, "- %s\n", key)
	}

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
		a.deps.Logger.Warn("context summarization failed, keeping original", "err", err)
		return messages
	}

	a.deps.Logger.Info("context summarized",
		"original_messages", len(history),
		"summary_len", len(summary),
	)

	// Формируем итоговое сообщение с явным списком уже вызванных инструментов
	content := fmt.Sprintf(
		"Investigation summary so far:\n%s\n\nALREADY CALLED (do not repeat these):\n%s\nContinue the investigation with NEW tool calls only.",
		summary,
		calledTools.String(),
	)

	return append(fixed, Message{
		Role:    "user",
		Content: content,
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

	// Подсказываем агенту точку входа — чтобы не тратить шаг на угадывание
	if node := al.Labels["node"]; node != "" {
		fmt.Fprintf(&sb, "\nPrimary resource: node/%s", node)
		fmt.Fprintf(&sb, "\nNote: this is a NODE alert. Start with describe_resource(kind=node, name=%s, namespace=''). Node resources do not require namespace.", node)
	} else if pod := al.Labels["pod"]; pod != "" {
		fmt.Fprintf(&sb, "\nPrimary resource: pod/%s in namespace %s", pod, al.Namespace)
		fmt.Fprintf(&sb, "\nFor Prometheus queries use EXACTLY: pod=\"%s\", namespace=\"%s\"", pod, al.Namespace)
	} else if deploy := al.Labels["deployment"]; deploy != "" {
		fmt.Fprintf(&sb, "\nPrimary resource: deployment/%s in namespace %s", deploy, al.Namespace)
	} else if pvc := al.Labels["persistentvolumeclaim"]; pvc != "" {
		fmt.Fprintf(&sb, "\nPrimary resource: persistentvolumeclaim/%s in namespace %s", pvc, al.Namespace)
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

// interpretMemoryMetrics разбирает вывод check_metrics и возвращает
// вывод для модели если утилизация памяти высокая.
func interpretMemoryMetrics(result string) string {
	for _, line := range strings.Split(result, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "utilization=") {
			continue
		}
		switch {
		case strings.Contains(line, "CRITICAL"):
			return `Memory utilization is CRITICAL (≥95% of limit). Pod is at immediate risk of OOMKill.
This IS the root cause. Provide your FINAL answer with confidence=high.
Root cause: "Container is using [X]Mi of [limit]Mi memory limit ([pct]% utilization), at immediate risk of OOMKill."
Actions: increase memory limit immediately (low risk), investigate memory leak.`
		case strings.Contains(line, "WARNING"):
			return `Memory utilization is HIGH (≥85% of limit). This confirms the PodHighMemoryUsage alert.
This IS the root cause. Provide your FINAL answer with confidence=high.
Root cause: "Container memory usage is high: [X]Mi of [limit]Mi limit ([pct]% utilization)."
Actions: increase memory limit, monitor for growth trend.`
		}
	}
	return ""
}