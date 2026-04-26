package k8s

import (
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
)

type Client struct {
	cs       *kubernetes.Clientset
	allowed  map[string]struct{}
}

func NewClient(namespaces []string) (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// Fallback для локальной разработки
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.Getenv("HOME") + "/.kube/config"
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("build kubeconfig: %w", err)
		}
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}

	allowed := make(map[string]struct{}, len(namespaces))
	for _, ns := range namespaces {
		allowed[ns] = struct{}{}
	}

	return &Client{cs: cs, allowed: allowed}, nil
}

// CheckAccess — проверяет доступ к каждому namespace при старте
func (c *Client) CheckAccess(ctx context.Context) map[string]string {
	report := make(map[string]string, len(c.allowed))
	for ns := range c.allowed {
		_, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{Limit: 1})
		if err != nil {
			report[ns] = "denied: " + err.Error()
		} else {
			report[ns] = "ok"
		}
	}
	return report
}

// DescribeResource — роутер по kind
func (c *Client) DescribeResource(ctx context.Context, kind, name, ns string) (string, error) {
	if err := c.checkNS(ns); err != nil {
		return "", err
	}
	switch strings.ToLower(kind) {
	case "pod":
		return c.describePod(ctx, name, ns)
	case "deployment", "deploy":
		return c.describeDeployment(ctx, name, ns)
	case "node":
		return c.describeNode(ctx, name)
	default:
		return "", fmt.Errorf("unsupported kind: %s", kind)
	}
}

// GetLogs — возвращает последние N строк логов контейнера
func (c *Client) GetLogs(ctx context.Context, pod, ns string, lines int64) ([]string, error) {
	if err := c.checkNS(ns); err != nil {
		return nil, err
	}

	req := c.cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		TailLines: &lines,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("stream logs: %w", err)
	}
	defer stream.Close()

	data, err := io.ReadAll(stream)
	if err != nil {
		return nil, fmt.Errorf("read logs: %w", err)
	}

	raw := strings.Split(string(data), "\n")
	result := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			result = append(result, line)
		}
	}
	return result, nil
}

// GetEvents — Warning events за последние N минут
func (c *Client) GetEvents(ctx context.Context, ns string) (string, error) {
	if err := c.checkNS(ns); err != nil {
		return "", err
	}

	events, err := c.cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning",
	})
	if err != nil {
		return "", fmt.Errorf("list events: %w", err)
	}

	var sb strings.Builder
	for _, e := range events.Items {
		fmt.Fprintf(&sb, "[%s] %s/%s: %s (×%d)\n",
			e.LastTimestamp.Format("15:04:05"),
			e.InvolvedObject.Kind,
			e.InvolvedObject.Name,
			e.Message,
			e.Count,
		)
	}
	return sb.String(), nil
}

// ListRelated — находит поды связанные с сервисом по имени
// Порядок поиска:
// 1. Service с таким именем → берём selector → ищем поды
// 2. Deployment с таким именем → берём selector → ищем поды
// 3. Поды с label app=<name> напрямую
func (c *Client) ListRelated(ctx context.Context, service, ns string) (string, error) {
	if err := c.checkNS(ns); err != nil {
		return "", err
	}

	var sb strings.Builder

	// 1. Ищем Service
	svc, err := c.cs.CoreV1().Services(ns).Get(ctx, service, metav1.GetOptions{})
	if err == nil && len(svc.Spec.Selector) > 0 {
		fmt.Fprintf(&sb, "Service %s/%s found\n", ns, service)
		fmt.Fprintf(&sb, "  ClusterIP: %s\n", svc.Spec.ClusterIP)
		fmt.Fprintf(&sb, "  Ports: ")
		for _, p := range svc.Spec.Ports {
			fmt.Fprintf(&sb, "%d/%s ", p.Port, p.Protocol)
		}
		fmt.Fprintf(&sb, "\n")

		// Ищем поды за этим сервисом
		selector := metav1.FormatLabelSelector(&metav1.LabelSelector{
			MatchLabels: svc.Spec.Selector,
		})
		pods, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: labelMapToSelector(svc.Spec.Selector),
		})
		if err == nil {
			fmt.Fprintf(&sb, "  Pods behind service (%d):\n", len(pods.Items))
			for _, p := range pods.Items {
				fmt.Fprintf(&sb, "    - %s phase=%s restarts=%d\n",
					p.Name, p.Status.Phase, totalRestarts(p))
			}
		}
		_ = selector
		return sb.String(), nil
	}

	// 2. Ищем Deployment
	deploy, err := c.cs.AppsV1().Deployments(ns).Get(ctx, service, metav1.GetOptions{})
	if err == nil {
		fmt.Fprintf(&sb, "Deployment %s/%s found\n", ns, service)
		fmt.Fprintf(&sb, "  Replicas: desired=%d ready=%d\n",
			*deploy.Spec.Replicas, deploy.Status.ReadyReplicas)

		if deploy.Spec.Selector != nil {
			pods, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
				LabelSelector: labelMapToSelector(deploy.Spec.Selector.MatchLabels),
			})
			if err == nil {
				fmt.Fprintf(&sb, "  Pods (%d):\n", len(pods.Items))
				for _, p := range pods.Items {
					fmt.Fprintf(&sb, "    - %s phase=%s restarts=%d\n",
						p.Name, p.Status.Phase, totalRestarts(p))
				}
			}
		}
		return sb.String(), nil
	}

	// 3. Fallback — ищем поды по label app=<name>
	pods, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + service,
	})
	if err == nil && len(pods.Items) > 0 {
		fmt.Fprintf(&sb, "Pods with label app=%s (%d):\n", service, len(pods.Items))
		for _, p := range pods.Items {
			fmt.Fprintf(&sb, "  - %s phase=%s restarts=%d\n",
				p.Name, p.Status.Phase, totalRestarts(p))
		}
		return sb.String(), nil
	}

	return fmt.Sprintf("No service, deployment or pods found for %q in namespace %q", service, ns), nil
}

// isSensitiveEnv — маскируем переменные которые могут содержать секреты
func isSensitiveEnv(name string) bool {
	name = strings.ToUpper(name)
	sensitive := []string{"PASSWORD", "SECRET", "TOKEN", "KEY", "CERT", "PRIVATE", "CREDENTIAL", "AUTH"}
	for _, s := range sensitive {
		if strings.Contains(name, s) {
			return true
		}
	}
	return false
}

func labelMapToSelector(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func totalRestarts(pod corev1.Pod) int32 {
	var total int32
	for _, cs := range pod.Status.ContainerStatuses {
		total += cs.RestartCount
	}
	return total
}

// --- private ---

func (c *Client) checkNS(ns string) error {
	if _, ok := c.allowed[ns]; !ok {
		return fmt.Errorf("namespace %q not in allowed list: skipped", ns)
	}
	return nil
}

func (c *Client) describePod(ctx context.Context, name, ns string) (string, error) {
	pod, err := c.cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get pod: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Pod: %s/%s\n", ns, name)
	fmt.Fprintf(&sb, "Phase: %s\n", pod.Status.Phase)
	fmt.Fprintf(&sb, "Node: %s\n", pod.Spec.NodeName)

	for _, cs := range pod.Status.ContainerStatuses {
		fmt.Fprintf(&sb, "\nContainer: %s\n", cs.Name)
		fmt.Fprintf(&sb, "  Ready: %v | RestartCount: %d\n", cs.Ready, cs.RestartCount)
		if t := cs.LastTerminationState.Terminated; t != nil {
			fmt.Fprintf(&sb, "  LastTermination: reason=%s exitCode=%d\n", t.Reason, t.ExitCode)
		}
		if cs.State.Waiting != nil {
			fmt.Fprintf(&sb, "  Waiting: reason=%s message=%s\n",
				cs.State.Waiting.Reason, cs.State.Waiting.Message)
		}
	}

	for _, c := range pod.Spec.Containers {
		fmt.Fprintf(&sb, "\nResources[%s]:\n", c.Name)
		fmt.Fprintf(&sb, "  limits:   cpu=%s memory=%s\n",
			c.Resources.Limits.Cpu(), c.Resources.Limits.Memory())
		fmt.Fprintf(&sb, "  requests: cpu=%s memory=%s\n",
			c.Resources.Requests.Cpu(), c.Resources.Requests.Memory())

		// ENV vars — часто содержат адреса сервисов, порты, названия секретов
		if len(c.Env) > 0 {
			fmt.Fprintf(&sb, "  env:\n")
			for _, e := range c.Env {
				if e.ValueFrom != nil {
					// Показываем откуда берётся значение, но не само значение
					if e.ValueFrom.SecretKeyRef != nil {
						fmt.Fprintf(&sb, "    %s=<secret:%s/%s>\n",
							e.Name,
							e.ValueFrom.SecretKeyRef.Name,
							e.ValueFrom.SecretKeyRef.Key,
						)
					} else if e.ValueFrom.ConfigMapKeyRef != nil {
						fmt.Fprintf(&sb, "    %s=<configmap:%s/%s>\n",
							e.Name,
							e.ValueFrom.ConfigMapKeyRef.Name,
							e.ValueFrom.ConfigMapKeyRef.Key,
						)
					}
				} else {
					// Маскируем чувствительные значения
					val := e.Value
					if isSensitiveEnv(e.Name) {
						val = "<masked>"
					}
					fmt.Fprintf(&sb, "    %s=%s\n", e.Name, val)
				}
			}
		}
	}

	return sb.String(), nil
}

func (c *Client) describeDeployment(ctx context.Context, name, ns string) (string, error) {
	d, err := c.cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get deployment: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Deployment: %s/%s\n", ns, name)
	fmt.Fprintf(&sb, "Replicas: desired=%d ready=%d available=%d\n",
		*d.Spec.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas)
	fmt.Fprintf(&sb, "Strategy: %s\n", d.Spec.Strategy.Type)

	for _, cond := range d.Status.Conditions {
		fmt.Fprintf(&sb, "Condition: %s=%s reason=%s\n",
			cond.Type, cond.Status, cond.Reason)
	}
	return sb.String(), nil
}

func (c *Client) describeNode(ctx context.Context, name string) (string, error) {
	node, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get node: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Node: %s\n", name)
	fmt.Fprintf(&sb, "Capacity:    cpu=%s memory=%s\n",
		node.Status.Capacity.Cpu(), node.Status.Capacity.Memory())
	fmt.Fprintf(&sb, "Allocatable: cpu=%s memory=%s\n",
		node.Status.Allocatable.Cpu(), node.Status.Allocatable.Memory())

	for _, cond := range node.Status.Conditions {
		fmt.Fprintf(&sb, "Condition: %s=%s\n", cond.Type, cond.Status)
	}
	return sb.String(), nil
}