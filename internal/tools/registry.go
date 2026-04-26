package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"math"
	"net/url"
	"strconv"

	"github.com/rus-99-pk/srengine/internal/agent"
	"github.com/rus-99-pk/srengine/internal/config"
	"github.com/rus-99-pk/srengine/internal/logs"
)

// K8sClient defines Kubernetes operations to prevent circular dependencies.
type K8sClient interface {
	DescribeResource(ctx context.Context, kind, name, ns string) (string, error)
	GetLogs(ctx context.Context, pod, ns string, lines int64) ([]string, error)
	GetEvents(ctx context.Context, ns string) (string, error)
	ListRelated(ctx context.Context, service, ns string) (string, error)
	ListPodsByNode(ctx context.Context, node string) (string, error)
	GetResourceYAML(ctx context.Context, kind, name, ns string) (string, error)
	GetHPA(ctx context.Context, name, ns string) (string, error)
}

// Tool defines the interface for all investigation tools.
type Tool interface {
	Name() string
	Description() string
	Execute(ctx context.Context, args map[string]any) (string, error)
}

// Registry holds and manages all available tools.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry initializes and populates the tool registry.
func NewRegistry(k8s K8sClient, dedup *logs.Deduplicator, cfg *config.Config) *Registry {
	r := &Registry{tools: make(map[string]Tool)}

	r.register(&describeResourceTool{k8s: k8s})
	r.register(&getLogsTool{k8s: k8s, dedup: dedup, cfg: cfg.Logs})
	r.register(&getEventsTool{k8s: k8s})
	r.register(&fetchRunbookTool{client: &http.Client{Timeout: 10 * time.Second}})
	r.register(&listRelatedTool{k8s: k8s})
	r.register(&listPodsByNodeTool{k8s: k8s})
	r.register(&getResourceYAMLTool{k8s: k8s})
	r.register(&getHPATool{k8s: k8s})

	if cfg.Metrics.PrometheusURL != "" {
		r.register(&checkMetricsTool{
			promURL: cfg.Metrics.PrometheusURL,
			client:  &http.Client{Timeout: cfg.Metrics.Timeout},
		})
	}

	return r
}

// register adds a new tool to the internal map.
func (r *Registry) register(t Tool) {
	r.tools[t.Name()] = t
}

// Execute routes the LLM action to the corresponding tool.
func (r *Registry) Execute(ctx context.Context, call *agent.ToolCall) (string, error) {
	t, ok := r.tools[call.Tool]
	if !ok {
		return "", fmt.Errorf("unknown tool: %q. Available: %s",
			call.Tool, r.toolNames())
	}
	return t.Execute(ctx, call.Args)
}

// Descriptions returns a formatted list of tools for the LLM system prompt.
func (r *Registry) Descriptions() string {
	var sb strings.Builder
	for _, t := range r.tools {
		fmt.Fprintf(&sb, "- %s: %s\n", t.Name(), t.Description())
	}
	return sb.String()
}

// toolNames returns a comma-separated string of registered tool names.
func (r *Registry) toolNames() string {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

// ── describe_resource ─────────────────────────────────────────────────────────

type describeResourceTool struct{ k8s K8sClient }

func (t *describeResourceTool) Name() string { return "describe_resource" }
func (t *describeResourceTool) Description() string {
	return "describe_resource(kind, name, namespace) — key fields of pod/deploy/node/pvc: state, restarts, resources, env, probes"
}
func (t *describeResourceTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	kind, _ := args["kind"].(string)
	name, _ := args["name"].(string)
	ns, _ := args["namespace"].(string)
	if kind == "" || name == "" {
		return "", fmt.Errorf("describe_resource requires kind, name")
	}
	if strings.ToLower(kind) != "node" && ns == "" {
		return "", fmt.Errorf("describe_resource requires namespace for kind=%s", kind)
	}
	return t.k8s.DescribeResource(ctx, kind, name, ns)
}

// ── get_logs ──────────────────────────────────────────────────────────────────

type getLogsTool struct {
	k8s   K8sClient
	dedup *logs.Deduplicator
	cfg   config.LogsConfig
}

func (t *getLogsTool) Name() string { return "get_logs" }
func (t *getLogsTool) Description() string {
	return "get_logs(name, namespace) — deduplicated ERROR/WARN log patterns from pod (falls back to previous container if restarted)"
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

// ── get_events ────────────────────────────────────────────────────────────────

type getEventsTool struct{ k8s K8sClient }

func (t *getEventsTool) Name() string { return "get_events" }
func (t *getEventsTool) Description() string {
	return "get_events(namespace) — Warning events from namespace (quota errors, scheduling failures, probe failures)"
}
func (t *getEventsTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	ns, _ := args["namespace"].(string)
	if ns == "" {
		return "", fmt.Errorf("get_events requires namespace")
	}
	return t.k8s.GetEvents(ctx, ns)
}

// ── fetch_runbook ─────────────────────────────────────────────────────────────

type fetchRunbookTool struct{ client *http.Client }

func (t *fetchRunbookTool) Name() string { return "fetch_runbook" }
func (t *fetchRunbookTool) Description() string {
	return "fetch_runbook(url) — fetch runbook content by URL, trimmed to 2000 chars"
}
func (t *fetchRunbookTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	u, _ := args["url"].(string)
	if u == "" {
		return "no runbook URL provided in alert context", nil
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "invalid runbook URL: must start with http:// or https://", nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return "", err
	}

	content := string(body)
	if len(content) > 2000 {
		content = content[:2000] + "\n[truncated]"
	}
	return content, nil
}

// ── list_related ──────────────────────────────────────────────────────────────

type listRelatedTool struct{ k8s K8sClient }

func (t *listRelatedTool) Name() string { return "list_related" }
func (t *listRelatedTool) Description() string {
	return "list_related(service, namespace) — find pods/deployments related to a service by name or label"
}
func (t *listRelatedTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	service, _ := args["service"].(string)
	ns, _ := args["namespace"].(string)
	if service == "" || ns == "" {
		return "", fmt.Errorf("list_related requires service, namespace")
	}
	return t.k8s.ListRelated(ctx, service, ns)
}

// ── list_pods_by_node ─────────────────────────────────────────────────────────

type listPodsByNodeTool struct{ k8s K8sClient }

func (t *listPodsByNodeTool) Name() string { return "list_pods_by_node" }
func (t *listPodsByNodeTool) Description() string {
	return "list_pods_by_node(node) — all pods on a node with namespace/phase/restarts; use after KubeNodeNotReady alerts"
}
func (t *listPodsByNodeTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	node, _ := args["node"].(string)
	if node == "" {
		return "", fmt.Errorf("list_pods_by_node requires node")
	}
	return t.k8s.ListPodsByNode(ctx, node)
}

// ── get_resource_yaml ─────────────────────────────────────────────────────────

type getResourceYAMLTool struct{ k8s K8sClient }

func (t *getResourceYAMLTool) Name() string { return "get_resource_yaml" }
func (t *getResourceYAMLTool) Description() string {
	return "get_resource_yaml(kind, name, namespace) — cleaned spec YAML without status/managedFields; use when describe_resource is not enough"
}
func (t *getResourceYAMLTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	kind, _ := args["kind"].(string)
	name, _ := args["name"].(string)
	ns, _ := args["namespace"].(string)
	if kind == "" || name == "" {
		return "", fmt.Errorf("get_resource_yaml requires kind, name")
	}
	if strings.ToLower(kind) != "node" && ns == "" {
		return "", fmt.Errorf("get_resource_yaml requires namespace for kind=%s", kind)
	}
	return t.k8s.GetResourceYAML(ctx, kind, name, ns)
}

// ── get_hpa ───────────────────────────────────────────────────────────────────

type getHPATool struct{ k8s K8sClient }

func (t *getHPATool) Name() string { return "get_hpa" }
func (t *getHPATool) Description() string {
	return "get_hpa(name, namespace) — HPA current/desired/min/max replicas, metric targets vs current values, conditions; accepts HPA name or target deployment name"
}
func (t *getHPATool) Execute(ctx context.Context, args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	ns, _ := args["namespace"].(string)
	if name == "" || ns == "" {
		return "", fmt.Errorf("get_hpa requires name, namespace")
	}
	return t.k8s.GetHPA(ctx, name, ns)
}

// ── check_metrics ─────────────────────────────────────────────────────────────

type checkMetricsTool struct {
	promURL string
	client  *http.Client
}

func (t *checkMetricsTool) Name() string { return "check_metrics" }
func (t *checkMetricsTool) Description() string {
	return `check_metrics(promql, range_minutes, limit_promql?) — query Prometheus, returns min/max/avg/last.` +
		` For memory alerts pass limit_promql to get utilization %:` +
		` limit_promql="kube_pod_container_resource_limits{pod=\"<name>\",namespace=\"<ns>\",resource=\"memory\"}"`
}

func (t *checkMetricsTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["promql"].(string)
	if query == "" {
		return "", fmt.Errorf("check_metrics requires promql")
	}

	rangeMin := 30
	if v, ok := args["range_minutes"]; ok {
		switch n := v.(type) {
		case float64:
			rangeMin = int(n)
		case int:
			rangeMin = n
		}
	}

	usageSeries, err := t.queryRange(ctx, query, rangeMin)
	if err != nil {
		return "", err
	}

	var limitSeries []promSeries
	if limitQuery, ok := args["limit_promql"].(string); ok && limitQuery != "" {
		limitSeries, _ = t.queryRange(ctx, limitQuery, rangeMin)
	}

	lowerQuery := strings.ToLower(query)
	isMemory := strings.Contains(lowerQuery, "memory") ||
		strings.Contains(lowerQuery, "working_set_bytes") ||
		strings.Contains(lowerQuery, "_rss") ||
		strings.Contains(lowerQuery, "_mem")

	return formatMetrics(usageSeries, limitSeries, rangeMin, isMemory), nil
}

// queryRange fetches range metrics from the Prometheus API.
func (t *checkMetricsTool) queryRange(ctx context.Context, query string, rangeMin int) ([]promSeries, error) {
	end := time.Now()
	start := end.Add(-time.Duration(rangeMin) * time.Minute)
	step := rangeMin * 60 / 60
	if step < 15 {
		step = 15
	}

	params := url.Values{}
	params.Set("query", query)
	params.Set("start", fmt.Sprintf("%d", start.Unix()))
	params.Set("end", fmt.Sprintf("%d", end.Unix()))
	params.Set("step", fmt.Sprintf("%ds", step))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		t.promURL+"/api/v1/query_range?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned %d", resp.StatusCode)
	}

	var promResp prometheusRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&promResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if promResp.Status != "success" {
		return nil, fmt.Errorf("prometheus error: %s", promResp.Error)
	}
	return promResp.Data.Result, nil
}

// promSeries represents a single time series returned by Prometheus.
type promSeries struct {
	Metric map[string]string `json:"metric"`
	Values [][]any           `json:"values"`
}

type prometheusRangeResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Data   struct {
		ResultType string       `json:"resultType"`
		Result     []promSeries `json:"result"`
	} `json:"data"`
}

// formatMetrics formats the series data into a human-readable summary.
// If limitSeries is provided, it computes utilization and adds a severity label.
func formatMetrics(usageSeries, limitSeries []promSeries, rangeMin int, isMemory bool) string {
	if len(usageSeries) == 0 {
		return fmt.Sprintf("No data found for query over last %d minutes", rangeMin)
	}

	// Use the last value from the first series as the limit.
	limitBytes := 0.0
	if len(limitSeries) > 0 {
		_, _, _, last, n := seriesStats(limitSeries[0].Values)
		if n > 0 {
			limitBytes = last
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Metrics over last %d minutes:\n", rangeMin)

	for _, s := range usageSeries {
		label := compactLabel(s.Metric)
		if label != "" {
			fmt.Fprintf(&sb, "  {%s}\n", label)
		}

		minV, maxV, avgV, lastV, n := seriesStats(s.Values)
		if n == 0 {
			fmt.Fprintf(&sb, "    no numeric data\n")
			continue
		}

		if isMemory {
			fmt.Fprintf(&sb, "    min=%.1fMi  max=%.1fMi  avg=%.1fMi  last=%.1fMi\n",
				toMiB(minV), toMiB(maxV), toMiB(avgV), toMiB(lastV))
			if limitBytes > 0 {
				pct := lastV / limitBytes * 100
				fmt.Fprintf(&sb, "    limit=%.1fMi  utilization=%.0f%%  → %s\n",
					toMiB(limitBytes), pct, memSeverity(pct))
			}
		} else {
			fmt.Fprintf(&sb, "    min=%.4g  max=%.4g  avg=%.4g  last=%.4g  (%d points)\n",
				minV, maxV, avgV, lastV, n)
		}
	}

	return sb.String()
}

// seriesStats calculates basic statistics from a raw Prometheus series payload.
func seriesStats(values [][]any) (minV, maxV, avgV, lastV float64, count int) {
	minV = 1e18
	var sum float64
	for _, pt := range values {
		if len(pt) < 2 {
			continue
		}
		s, ok := pt[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(v) {
			continue
		}
		sum += v
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
		lastV = v
		count++
	}
	if count > 0 {
		avgV = sum / float64(count)
	} else {
		minV = 0
	}
	return
}

// compactLabel filters out noisy cadvisor labels to leave only meaningful identifiers.
func compactLabel(metric map[string]string) string {
	meaningful := []string{"pod", "container", "namespace", "node", "persistentvolumeclaim"}
	var parts []string
	for _, k := range meaningful {
		if v, ok := metric[k]; ok {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, ", ")
}

func toMiB(b float64) float64        { return b / (1024 * 1024) }
func memSeverity(pct float64) string {
	switch {
	case pct >= 95:
		return "CRITICAL"
	case pct >= 85:
		return "WARNING"
	case pct >= 70:
		return "ELEVATED"
	default:
		return "OK"
	}
}