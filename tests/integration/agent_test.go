package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ScenarioResult — результат расследования из лога нотифаера
type ScenarioResult struct {
	RootCause  string   `json:"root_cause"`
	Confidence string   `json:"confidence"`
	Summary    string   `json:"summary"`
	StepsUsed  int      `json:"steps_used"`
	Actions    []Action `json:"actions"`
	ParseError bool     `json:"parse_error"`
}

type Action struct {
	Priority    int    `json:"priority"`
	Description string `json:"description"`
	Command     string `json:"command"`
	RiskLevel   string `json:"risk_level"`
}

// Scenario — описание одного тестового сценария
type Scenario struct {
	Name         string
	Namespace    string
	ManifestPath string

	AlertName   string
	AlertLabels map[string]string

	WantRootCauseContains []string
	WantConfidence        []string
	WantActionsContain    []string
	WantMaxSteps          int

	WaitForCrash time.Duration
	SetupFunc func(t *testing.T) map[string]string
}

var scenarios = []Scenario{
	{
		Name:         "test-cascade",
		Namespace:    "test-cascade",
		ManifestPath: "../scenarios/test-cascade/deployment.yaml",
		AlertName:    "KubePodCrashLooping",
		AlertLabels: map[string]string{
			"alertname": "KubePodCrashLooping",
			"namespace": "test-cascade",
			"pod":       "frontend",
		},
		WantRootCauseContains: []string{"redis", "backend", "connection"},
		WantConfidence:        []string{"high", "medium", "low"},
		WantActionsContain:    []string{"redis", "backend"},
		WantMaxSteps:          12,
		WaitForCrash:          60 * time.Second,
	},
	{
		Name:         "test-oom",
		Namespace:    "test-oom",
		ManifestPath: "../scenarios/test-oom/deployment.yaml",
		AlertName:    "KubePodCrashLooping",
		AlertLabels: map[string]string{
			"alertname": "KubePodCrashLooping",
			"namespace": "test-oom",
			"pod":       "memory-hog",
		},
		WantRootCauseContains: []string{"OOMKilled", "memory", "oom", "limit"},
		WantConfidence:        []string{"high", "medium"},
		WantActionsContain:    []string{"memory", "limit"},
		WantMaxSteps:          6,
		WaitForCrash:          20 * time.Second,
	},
	{
		Name:         "test-crashloop",
		Namespace:    "test-crashloop",
		ManifestPath: "../scenarios/test-crashloop/deployment.yaml",
		AlertName:    "KubePodCrashLooping",
		AlertLabels: map[string]string{
			"alertname": "KubePodCrashLooping",
			"namespace": "test-crashloop",
			"pod":       "app",
		},
		WantRootCauseContains: []string{"postgres", "db-service", "database", "connection", "missing"},
		WantConfidence:        []string{"high", "medium", "low"},
		WantActionsContain:    []string{"postgres", "db-service", "database"},
		WantMaxSteps:          12,
		WaitForCrash:          20 * time.Second,
	},
	{
		Name:         "test-quota",
		Namespace:    "test-quota",
		ManifestPath: "../scenarios/test-quota/deployment.yaml",
		AlertName:    "KubeDeploymentReplicasMismatch",
		AlertLabels: map[string]string{
			"alertname":  "KubeDeploymentReplicasMismatch",
			"namespace":  "test-quota",
			"deployment": "quota-victim",
		},
		WantRootCauseContains: []string{"quota", "exceeded", "ResourceQuota", "resource"},
		WantConfidence:        []string{"high", "medium"},
		WantActionsContain:    []string{"quota", "resource"},
		WantMaxSteps:          12,
		WaitForCrash:          15 * time.Second,
	},
	{
		Name:         "test-liveness",
		Namespace:    "test-liveness",
		ManifestPath: "../scenarios/test-liveness/deployment.yaml",
		AlertName:    "LivenessProbeFailed",
		AlertLabels: map[string]string{
			"alertname": "LivenessProbeFailed",
			"namespace": "test-liveness",
			"pod":       "bad-liveness",
		},
		WantRootCauseContains: []string{"liveness", "probe", "failed", "restart"},
		WantConfidence:        []string{"high", "medium"},
		WantActionsContain:    []string{"liveness", "probe", "command"},
		WantMaxSteps:          6,
		WaitForCrash:          30 * time.Second,
	},
	{
		Name:         "test-pv",
		Namespace:    "test-pv",
		ManifestPath: "../scenarios/test-pv/deployment.yaml",
		AlertName:    "KubePersistentVolumeFillingUp",
		AlertLabels: map[string]string{
			"alertname":             "KubePersistentVolumeFillingUp",
			"namespace":             "test-pv",
			"persistentvolumeclaim": "small-pvc",
		},
		WantRootCauseContains: []string{"disk", "volume", "pvc", "storage", "full", "filling", "capacity"},
		WantConfidence:        []string{"high", "medium"},
		WantActionsContain:    []string{"disk", "volume", "storage", "pvc"},
		WantMaxSteps:          8,
		WaitForCrash:          40 * time.Second,
	},
	{
		Name:         "test-node-notready",
		Namespace:    "test-node-notready",
		ManifestPath: "../scenarios/test-node-notready/deployment.yaml",
		AlertName:    "KubeNodeNotReady",
		AlertLabels:  map[string]string{
			"alertname": "KubeNodeNotReady",
		},
		WantRootCauseContains: []string{"node", "NotReady", "kubelet", "Kubelet", "heartbeat", "stopped"},
		WantConfidence:        []string{"high", "medium"},
		WantActionsContain:    []string{"node", "kubelet", "kubectl"},
		WantMaxSteps:          8,
		WaitForCrash: 3 * time.Minute,
        SetupFunc: func(t *testing.T) map[string]string {
            node := getWorkerNode(t)
            t.Logf("selected worker node: %s", node)
            cordonAndDrainNode(t, node)
            return map[string]string{"node": node}
        },
	},
	{
		Name:         "test-high-memory",
		Namespace:    "test-high-memory",
		ManifestPath: "../scenarios/test-high-memory/deployment.yaml",
		AlertName:    "PodHighMemoryUsage",
		AlertLabels: map[string]string{
			"alertname": "PodHighMemoryUsage",
			"namespace": "test-high-memory",
			"pod":       "memory-pressure",
		},
		WantRootCauseContains: []string{"memory", "limit", "90", "pressure", "usage"},
		WantConfidence:        []string{"high", "medium"},
		WantActionsContain:    []string{"memory", "limit"},
		WantMaxSteps:          8,
		// Ждём дольше — Prometheus должен успеть scrape-нуть метрики (3+ цикла по 15s)
		WaitForCrash: 90 * time.Second,
	},
}

func TestScenarios(t *testing.T) {
	webhookURL := getEnv("AGENT_WEBHOOK_URL", "http://localhost:8080/webhook")
	resultDir := getEnv("RESULT_DIR", t.TempDir())

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			// Параллельно не запускаем — агент обрабатывает последовательно
			runScenario(t, sc, webhookURL, resultDir)
		})
	}
}

func runScenario(t *testing.T, sc Scenario, webhookURL, resultDir string) {
	t.Helper()
	t.Logf("=== Scenario: %s ===", sc.Name)

	// 1. Применяем манифест
	manifestPath := resolveManifestPath(t, sc.ManifestPath)
	applyManifest(t, manifestPath)
	if sc.SetupFunc != nil {
		extraLabels := sc.SetupFunc(t)
		for k, v := range extraLabels {
			sc.AlertLabels[k] = v
		}
	}

	// 2. Cleanup после теста
	t.Cleanup(func() {
		t.Logf("cleaning up namespace %s", sc.Namespace)
		deleteNamespace(t, sc.Namespace)
	})

	// 3. Ждём пока поды войдут в CrashLoop / нужное состояние
	t.Logf("waiting %s for pods to crash...", sc.WaitForCrash)
	time.Sleep(sc.WaitForCrash)

	// 4. Если в labels указан pod без хэша — резолвим реальное имя пода
	if podPrefix, ok := sc.AlertLabels["pod"]; ok {
		realPod := findPodByPrefix(t, sc.Namespace, podPrefix)
		if realPod != "" {
			sc.AlertLabels["pod"] = realPod
			t.Logf("resolved pod name: %s", realPod)
		}
	}

	// 5. Отправляем алерт агенту
	result := sendAlertAndWait(t, webhookURL, sc)

	// 6. Сохраняем результат
	saveResult(t, resultDir, sc.Name, result)

	// 7. Assertions
	assertResult(t, sc, result)
}

// sendAlertAndWait — отправляет алерт и ждёт завершения расследования
func sendAlertAndWait(t *testing.T, webhookURL string, sc Scenario) *ScenarioResult {
	t.Helper()

	fingerprint := fmt.Sprintf("%s-%d", sc.Name, time.Now().Unix())
	payload := buildAlertPayload(sc.AlertLabels, fingerprint)

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal alert payload: %v", err)
	}

	t.Logf("sending alert to %s (fingerprint=%s)", webhookURL, fingerprint)
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("send alert: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook returned %d", resp.StatusCode)
	}

	// Ждём завершения расследования — агент работает асинхронно
	// Таймаут = InvestigTimeout из конфига (по умолчанию 5 минут)
	timeout := getEnvDuration("TEST_INVESTIG_TIMEOUT", 8*time.Minute)
	result, err := waitForResult(t, webhookURL, fingerprint, timeout)
	if err != nil {
		t.Fatalf("wait for result: %v", err)
	}
	return result
}

// waitForResult — опрашивает /result эндпоинт пока не получит ответ
// Агент должен реализовать GET /result?fingerprint=<fp> → ScenarioResult JSON
func waitForResult(t *testing.T, baseURL, fingerprint string, timeout time.Duration) (*ScenarioResult, error) {
	t.Helper()

	resultURL := strings.Replace(baseURL, "/webhook", "/result", 1)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		<-ticker.C

		url := fmt.Sprintf("%s?fingerprint=%s", resultURL, fingerprint)
		resp, err := http.Get(url)
		if err != nil {
			t.Logf("polling result: %v (retrying...)", err)
			continue
		}

		if resp.StatusCode == http.StatusAccepted {
			// 202 = расследование ещё идёт
			resp.Body.Close()
			t.Logf("investigation in progress...")
			continue
		}

		if resp.StatusCode == http.StatusOK {
			var result ScenarioResult
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				resp.Body.Close()
				return nil, fmt.Errorf("decode result: %w", err)
			}
			resp.Body.Close()
			return &result, nil
		}

		resp.Body.Close()
		t.Logf("unexpected status %d from result endpoint", resp.StatusCode)
	}

	return nil, fmt.Errorf("timeout waiting for investigation result after %s", timeout)
}

// assertResult — проверяет результат расследования
func assertResult(t *testing.T, sc Scenario, result *ScenarioResult) {
	t.Helper()

	t.Logf("--- Result ---")
	t.Logf("root_cause:  %s", result.RootCause)
	t.Logf("confidence:  %s", result.Confidence)
	t.Logf("steps_used:  %d", result.StepsUsed)
	t.Logf("parse_error: %v", result.ParseError)
	t.Logf("summary:     %s", result.Summary)

	if result.ParseError {
		t.Error("FAIL: parse_error=true, model returned invalid JSON")
	}

	// Root cause должен содержать хотя бы одно из ожидаемых слов
	if len(sc.WantRootCauseContains) > 0 {
		rcLower := strings.ToLower(result.RootCause)
		found := false
		for _, want := range sc.WantRootCauseContains {
			if strings.Contains(rcLower, strings.ToLower(want)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("FAIL: root_cause %q does not contain any of %v",
				result.RootCause, sc.WantRootCauseContains)
		}
	}

	// Confidence должен быть из допустимых значений
	if len(sc.WantConfidence) > 0 {
		confOK := false
		for _, want := range sc.WantConfidence {
			if result.Confidence == want {
				confOK = true
				break
			}
		}
		if !confOK {
			t.Errorf("FAIL: confidence=%q, want one of %v",
				result.Confidence, sc.WantConfidence)
		}
	}

	// Actions должны упоминать хотя бы одно из ожидаемых слов
	if len(sc.WantActionsContain) > 0 && len(result.Actions) > 0 {
		actionsText := strings.Builder{}
		for _, a := range result.Actions {
			actionsText.WriteString(strings.ToLower(a.Description))
			actionsText.WriteString(" ")
			actionsText.WriteString(strings.ToLower(a.Command))
			actionsText.WriteString(" ")
		}
		text := actionsText.String()

		found := false
		for _, want := range sc.WantActionsContain {
			if strings.Contains(text, strings.ToLower(want)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("FAIL: actions do not mention any of %v\nactions text: %s",
				sc.WantActionsContain, text)
		}
	}

	// Шаги не должны превышать лимит
	if sc.WantMaxSteps > 0 && result.StepsUsed > sc.WantMaxSteps {
		t.Errorf("FAIL: steps_used=%d exceeds max=%d", result.StepsUsed, sc.WantMaxSteps)
	}
}

// --- kubectl helpers ---

func applyManifest(t *testing.T, path string) {
	t.Helper()
	t.Logf("applying manifest: %s", path)
	cmd := exec.Command("kubectl", "apply", "-f", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl apply failed: %v\n%s", err, out)
	}
	t.Logf("kubectl apply: %s", strings.TrimSpace(string(out)))
}

func deleteNamespace(t *testing.T, ns string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "delete", "namespace", ns, "--ignore-not-found")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("WARN: kubectl delete namespace %s: %v\n%s", ns, err, out)
		return
	}
	t.Logf("namespace %s deleted", ns)
}

// findPodByPrefix — находит реальное имя пода по префиксу (например "frontend" → "frontend-7c76bd67f-8t7h5")
func findPodByPrefix(t *testing.T, ns, prefix string) string {
	t.Helper()
	cmd := exec.Command("kubectl", "get", "pods", "-n", ns,
		"--no-headers", "-o", "custom-columns=NAME:.metadata.name")
	out, err := cmd.Output()
	if err != nil {
		t.Logf("WARN: get pods in %s: %v", ns, err)
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if strings.HasPrefix(name, prefix) {
			return name
		}
	}
	return ""
}

// --- payload builder ---

func buildAlertPayload(labels map[string]string, fingerprint string) map[string]any {
	return map[string]any{
		"alerts": []map[string]any{
			{
				"status":      "firing",
				"labels":      labels,
				"annotations": map[string]string{},
				"startsAt":    time.Now().UTC().Format(time.RFC3339),
				"fingerprint": fingerprint,
			},
		},
	}
}

// --- result storage ---

func saveResult(t *testing.T, dir, name string, result *ScenarioResult) {
	t.Helper()
	path := filepath.Join(dir, name+".json")
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Logf("WARN: marshal result: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Logf("WARN: write result file: %v", err)
		return
	}
	t.Logf("result saved: %s", path)
}

// --- helpers ---

func resolveManifestPath(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("resolve manifest path %q: %v", rel, err)
	}
	return abs
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
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

func cordonAndDrainNode(t *testing.T, nodeName string) {
	t.Helper()

	// Cordon чтобы новые поды не шедулились
	cmd := exec.Command("kubectl", "cordon", nodeName)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cordon node: %v\n%s", err, out)
	}

	// Приостанавливаем docker контейнер — kubelet перестаёт слать heartbeat
	// Нода уйдёт в NotReady через node-monitor-grace-period (~40s)
	t.Logf("pausing docker container %s", nodeName)
	cmd = exec.Command("docker", "pause", nodeName)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker pause: %v\n%s", err, out)
	}

	t.Cleanup(func() {
		t.Logf("unpausing docker container %s", nodeName)
		_ = exec.Command("docker", "unpause", nodeName).Run()
		waitForNodeReady(t, nodeName, 3*time.Minute)
		_ = exec.Command("kubectl", "uncordon", nodeName).Run()
	})
}

func waitForNodeReady(t *testing.T, nodeName string, timeout time.Duration) {
    t.Helper()
    deadline := time.Now().Add(timeout)
    for time.Now().Before(deadline) {
        cmd := exec.Command("kubectl", "get", "node", nodeName,
            "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
        out, err := cmd.Output()
        if err == nil && strings.TrimSpace(string(out)) == "True" {
            t.Logf("node %s is Ready again", nodeName)
            return
        }
        time.Sleep(5 * time.Second)
    }
    t.Logf("WARN: node %s did not return to Ready within %s", nodeName, timeout)
}

func getWorkerNode(t *testing.T) string {
    t.Helper()
    cmd := exec.Command("kubectl", "get", "nodes",
        "--no-headers",
        "-o", "custom-columns=NAME:.metadata.name,ROLE:.metadata.labels.node-role\\.kubernetes\\.io/control-plane",
        "--selector=!node-role.kubernetes.io/control-plane",
    )
    out, err := cmd.Output()
    if err != nil {
        t.Fatalf("get worker nodes: %v", err)
    }
    for _, line := range strings.Split(string(out), "\n") {
        name := strings.Fields(line)
        if len(name) > 0 && name[0] != "" {
            return name[0]
        }
    }
    t.Fatal("no worker nodes found")
    return ""
}