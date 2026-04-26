package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/your-org/ai-sre/internal/agent"
	"github.com/your-org/ai-sre/internal/config"
	"github.com/your-org/ai-sre/internal/logs"
)

// K8sClient — интерфейс чтобы не импортировать k8s пакет циклически
type K8sClient interface {
	DescribeResource(ctx context.Context, kind, name, ns string) (string, error)
	GetLogs(ctx context.Context, pod, ns string, lines int64) ([]string, error)
	GetEvents(ctx context.Context, ns string) (string, error)
	ListRelated(ctx context.Context, service, ns string) (string, error)
}

// Tool — интерфейс для каждого инструмента
type Tool interface {
	Name() string
	Description() string
	Execute(ctx context.Context, args map[string]any) (string, error)
}

// Registry — реестр всех инструментов
type Registry struct {
	tools map[string]Tool
}

func NewRegistry(k8s K8sClient, dedup *logs.Deduplicator, cfg *config.Config) *Registry {
	r := &Registry{tools: make(map[string]Tool)}

	r.register(&describeResourceTool{k8s: k8s})
	r.register(&getLogsTool{k8s: k8s, dedup: dedup, cfg: cfg.Logs})
	r.register(&getEventsTool{k8s: k8s})
	r.register(&fetchRunbookTool{client: &http.Client{Timeout: 10 * time.Second}})
	r.register(&listRelatedTool{k8s: k8s})

	return r
}

func (r *Registry) register(t Tool) {
	r.tools[t.Name()] = t
}

func (r *Registry) Execute(ctx context.Context, call *agent.ToolCall) (string, error) {
	t, ok := r.tools[call.Tool]
	if !ok {
		return "", fmt.Errorf("unknown tool: %q. Available: %s",
			call.Tool, r.toolNames())
	}
	return t.Execute(ctx, call.Args)
}

// Descriptions — список инструментов для system prompt
func (r *Registry) Descriptions() string {
	var sb strings.Builder
	for _, t := range r.tools {
		fmt.Fprintf(&sb, "- %s: %s\n", t.Name(), t.Description())
	}
	return sb.String()
}

func (r *Registry) toolNames() string {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

// --- describe_resource ---

type describeResourceTool struct{ k8s K8sClient }

func (t *describeResourceTool) Name() string { return "describe_resource" }
func (t *describeResourceTool) Description() string {
	return "describe_resource(kind, name, namespace) — get pod/deploy/node details"
}
func (t *describeResourceTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	kind, _ := args["kind"].(string)
	name, _ := args["name"].(string)
	ns, _ := args["namespace"].(string)
	if kind == "" || name == "" || ns == "" {
		return "", fmt.Errorf("describe_resource requires kind, name, namespace")
	}
	return t.k8s.DescribeResource(ctx, kind, name, ns)
}

// --- get_logs ---

type getLogsTool struct {
	k8s  K8sClient
	dedup *logs.Deduplicator
	cfg  config.LogsConfig
}

func (t *getLogsTool) Name() string { return "get_logs" }
func (t *getLogsTool) Description() string {
	return "get_logs(name, namespace) — get deduplicated container logs (ERROR/WARN only)"
}
func (t *getLogsTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	ns, _ := args["namespace"].(string)
	if name == "" || ns == "" {
		return "", fmt.Errorf("get_logs requires name, namespace")
	}

	lines, err := t.k8s.GetLogs(ctx, name, ns, int64(t.cfg.MaxLines))
	if err != nil {
		return "", err
	}

	patterns := t.dedup.Process(lines)
	if len(patterns) == 0 {
		return "No ERROR/WARN log patterns found", nil
	}

	return t.dedup.FormatForLLM(patterns), nil
}

// --- get_events ---

type getEventsTool struct{ k8s K8sClient }

func (t *getEventsTool) Name() string { return "get_events" }
func (t *getEventsTool) Description() string {
	return "get_events(namespace) — get Warning events from namespace"
}
func (t *getEventsTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	ns, _ := args["namespace"].(string)
	if ns == "" {
		return "", fmt.Errorf("get_events requires namespace")
	}
	return t.k8s.GetEvents(ctx, ns)
}

// --- fetch_runbook ---

type fetchRunbookTool struct{ client *http.Client }

func (t *fetchRunbookTool) Name() string { return "fetch_runbook" }
func (t *fetchRunbookTool) Description() string {
	return "fetch_runbook(url) — fetch runbook content by URL"
}
func (t *fetchRunbookTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	url, _ := args["url"].(string)
	if url == "" {
		return "no runbook URL provided in alert context", nil
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "invalid runbook URL: must start with http:// or https://", nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192)) // max 8KB
	if err != nil {
		return "", err
	}

	// Обрезаем до 2000 символов чтобы не перегружать контекст
	content := string(body)
	if len(content) > 2000 {
		content = content[:2000] + "\n[truncated]"
	}
	return content, nil
}

// --- list_related ---

type listRelatedTool struct{ k8s K8sClient }

func (t *listRelatedTool) Name() string { return "list_related" }
func (t *listRelatedTool) Description() string {
	return "list_related(service, namespace) — find pods/deployments related to a service by name"
}
func (t *listRelatedTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	service, _ := args["service"].(string)
	ns, _ := args["namespace"].(string)
	if service == "" || ns == "" {
		return "", fmt.Errorf("list_related requires service, namespace")
	}
	return t.k8s.ListRelated(ctx, service, ns)
}