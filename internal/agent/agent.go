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
	Thought string    `json:"thought"`
	Action  *ToolCall `json:"action,omitempty"`
	Answer  *Report   `json:"answer,omitempty"`
}

type Action struct {
	Priority    int    `json:"priority"`
	Description string `json:"description"`
	Command     string `json:"command,omitempty"`
	RiskLevel   string `json:"risk_level"` // low | medium | high
}

type Report struct {
	Summary           string   `json:"summary"`
	RootCause         string   `json:"root_cause"`
	Confidence        string   `json:"confidence"` // high | medium | low
	Actions           []Action `json:"actions"`
	SkippedNamespaces []string `json:"skipped_namespaces,omitempty"`
	StepsUsed         int      `json:"steps_used"`
	Duration          string   `json:"duration"`
	AlertName         string   `json:"alert_name"`
	Namespace         string   `json:"namespace"`
	ParseError        bool     `json:"parse_error,omitempty"`
	RawResponse       string   `json:"raw_response,omitempty"`
}

// --- Interfaces ---

type LLMProvider interface {
	Complete(ctx context.Context, messages []Message) (string, error)
	Name() string
}

type ToolRegistry interface {
	Execute(ctx context.Context, call *ToolCall) (string, error)
	Descriptions() string // returns tool list for system prompt
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

// Investigate is the main entry point; returns (any, error) for server compatibility.
func (a *Agent) Investigate(ctx context.Context, al *alert.Alert) (any, error) {
	start := time.Now()
	log := a.deps.Logger.With("alert", al.Name, "namespace", al.Namespace)
	log.Info("starting investigation")

	ctx, cancel := context.WithTimeout(ctx, a.deps.Config.InvestigTimeout)
	defer cancel()

	report, err := a.runReAct(ctx, al)
	if err != nil {
		log.Error("react loop failed", "err", err)
		// Return partial report even on failure
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

	// Send report to all configured notifiers
	if err := a.deps.Notifier.Send(ctx, report); err != nil {
		log.Error("failed to send notification", "err", err)
	}

	return report, nil
}

// runReAct executes the Think → Act → Observe loop until answer or maxSteps.
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

		// Warn the model when steps are running out
		stepsLeft := a.deps.Config.MaxSteps - step
		if stepsLeft == 2 {
			messages = append(messages, Message{
				Role:    "user",
				Content: "WARNING: only 2 steps remaining. If you have enough data — provide your answer now. Otherwise make one final tool call.",
			})
		}

		// Think: ask the model
		raw, err := a.completeWithRetry(ctx, messages)
		if err != nil {
			return nil, fmt.Errorf("llm complete step %d: %w", step, err)
		}

		// Parse the model response
		ta, parseErr := parseResponse(raw)
		if parseErr != nil {
			a.deps.Logger.Error("failed to parse model response",
				"step", step+1,
				"raw", raw,
				"err", parseErr,
			)
			// Return partial report if parsing failed after retries
			return &Report{
				Summary:     "Failed to parse model response",
				Confidence:  "low",
				StepsUsed:   step + 1,
				ParseError:  true,
				RawResponse: raw,
			}, nil
		}

		// Append assistant turn to conversation history
		messages = append(messages, Message{Role: "assistant", Content: raw})

		// Answer: model decided it has enough data
		if ta.Answer != nil {
			ta.Answer.StepsUsed = step + 1
			return ta.Answer, nil
		}

		// Act: execute the requested tool call
		if ta.Action == nil {
			return nil, fmt.Errorf("step %d: no action and no answer", step)
		}

		// Deduplicate: skip identical tool calls to prevent loops
		callKey := fmt.Sprintf("%s:%v", ta.Action.Tool, ta.Action.Args)
		if _, seen := seenCalls[callKey]; seen {
			consecutiveDupes++
			a.deps.Logger.Warn("duplicate tool call skipped",
				"tool", ta.Action.Tool,
				"args", ta.Action.Args,
				"consecutive", consecutiveDupes,
			)

			// Force final answer after first consecutive duplicate
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

		// Observe: append tool result to conversation history
		toolResult, _ := json.Marshal(map[string]string{
			"tool":   ta.Action.Tool,
			"result": result,
		})
		messages = append(messages, Message{
			Role:    "user",
			Content: string(toolResult),
		})

		// Interpreter: help the model conclude when memory utilization is high
		if ta.Action.Tool == "check_metrics" {
			if hint := interpretMemoryMetrics(result); hint != "" {
				messages = append(messages, Message{Role: "user", Content: hint})
			}
		}

		// Detector: quota exceeded — force final answer immediately
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
		}

		// Detector: node NotReady — kubelet heartbeat lost
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

		// Detector: liveness probe with always-failing exec command
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

		// Detector: disk full keywords in pod logs
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

		// Detector: PVC has a mounted pod — steer model to check its logs
		if ta.Action.Tool == "describe_resource" &&
			strings.Contains(result, "MountedBy:") {
			// Extract pod name from "- pod-name (phase=Running..." line
			mountedPod := ""
			for _, line := range strings.Split(result, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "- ") {
					part := strings.TrimPrefix(line, "- ")
					if idx := strings.Index(part, " ("); idx > 0 {
						mountedPod = part[:idx]
					}
				}
			}
			if mountedPod != "" {
				// Extract namespace from "PVC: namespace/name" line
				ns := ""
				for _, line := range strings.Split(result, "\n") {
					if strings.HasPrefix(line, "PVC: ") {
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
							// Model agreed to call get_logs — continue the loop
							messages = append(messages, Message{Role: "assistant", Content: raw})
						}
					}
				}
			}
		}

		// Detector: OOMKilled + empty logs (expected) — conclude immediately
		if ta.Action.Tool == "get_logs" &&
			strings.TrimSpace(result) == "No ERROR/WARN log patterns found" &&
			strings.Contains(al.Name, "CrashLoop") {
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

		// Detector: PodHighMemoryUsage — call check_metrics directly to avoid model typos in pod name
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

			// Bypass the model to avoid pod name typos — call check_metrics directly
			forcedCall := &ToolCall{
				Tool: "check_metrics",
				Args: map[string]any{
					"promql":        fmt.Sprintf(`container_memory_working_set_bytes{pod="%s",namespace="%s"}`, podName, ns),
					"limit_promql":  fmt.Sprintf(`kube_pod_container_resource_limits{pod="%s",namespace="%s",resource="memory"}`, podName, ns),
					"range_minutes": 15,
				},
			}
			metricsResult, metricsErr := a.deps.Tools.Execute(ctx, forcedCall)
			if metricsErr != nil {
				metricsResult = fmt.Sprintf("error: %s", metricsErr.Error())
			}
			a.deps.Logger.Info("forced check_metrics for high memory", "pod", podName, "result", metricsResult)

			toolResult, _ := json.Marshal(map[string]string{
				"tool":   "check_metrics",
				"result": metricsResult,
			})
			messages = append(messages, Message{Role: "user", Content: string(toolResult)})

			if hint := interpretMemoryMetrics(metricsResult); hint != "" {
				messages = append(messages, Message{Role: "user", Content: hint})
			}
		}

		// Summarize conversation history every N steps to stay within context budget
		if a.deps.Config.SummarizeEvery > 0 &&
			(step+1)%a.deps.Config.SummarizeEvery == 0 {
			messages = a.summarizeContext(ctx, messages, seenCalls)
		}
	}

	// Max steps reached — ask for best-effort final answer
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

// summarizeContext compresses ReAct history into a single summary message,
// keeping system prompt and alert context intact.
func (a *Agent) summarizeContext(ctx context.Context, messages []Message, seenCalls map[string]struct{}) []Message {
	// Always keep system prompt and alert context (first two messages)
	if len(messages) <= 2 {
		return messages
	}

	fixed := messages[:2]
	history := messages[2:]

	// Build list of already-called tools to include in the summary
	var calledTools strings.Builder
	for key := range seenCalls {
		fmt.Fprintf(&calledTools, "- %s\n", key)
	}

	// Assemble history text for the summarization request
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

// completeWithRetry retries LLM completion up to maxRetries times on invalid JSON.
func (a *Agent) completeWithRetry(ctx context.Context, messages []Message) (string, error) {
	const maxRetries = 2
	var lastRaw string
	for attempt := 0; attempt <= maxRetries; attempt++ {
		msgs := messages
		if attempt > 0 {
			// Remind the model about the required output format
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
	return lastRaw, nil // return raw response; caller handles parse error
}

// loadPrompt reads the system prompt from disk and substitutes template variables.
func (a *Agent) loadPrompt(al *alert.Alert) (string, error) {
	data, err := os.ReadFile(a.deps.Config.PromptPath)
	if err != nil {
		return "", fmt.Errorf("read prompt file %s: %w", a.deps.Config.PromptPath, err)
	}

	prompt := string(data)
	prompt = strings.ReplaceAll(prompt, "{{TOOLS}}", a.deps.Tools.Descriptions())
	return prompt, nil
}

// buildAlertContext constructs the first user message with alert metadata.
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

	// Hint the model about the primary resource to avoid wasting a step
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

// parseResponse parses and validates the JSON model response.
func parseResponse(raw string) (*ThoughtAction, error) {
	// Strip markdown fences if the model wrapped the JSON
	clean := strings.TrimSpace(raw)
	clean = strings.TrimPrefix(clean, "```json")
	clean = strings.TrimPrefix(clean, "```")
	clean = strings.TrimSuffix(clean, "```")
	clean = strings.TrimSpace(clean)

	// Extract the first balanced JSON object — models sometimes emit trailing garbage
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

// extractFirstJSON returns the first balanced JSON object found in s.
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

// interpretMemoryMetrics parses check_metrics output and returns a conclusion hint
// when memory utilization crosses WARNING or CRITICAL thresholds.
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