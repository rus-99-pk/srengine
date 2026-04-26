package k8s

import (
	"context"
	"fmt"
	"io"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"sigs.k8s.io/yaml"
)

// Client acts as a wrapper around the official Kubernetes clientset.
type Client struct {
	cs      *kubernetes.Clientset
	allowed map[string]struct{}
}

// NewClient initializes the Kubernetes client config based on cluster context.
func NewClient(namespaces []string) (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
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

// CheckAccess verifies RBAC permissions for all configured namespaces on startup.
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

// DescribeResource routes the describe request based on the resource kind.
func (c *Client) DescribeResource(ctx context.Context, kind, name, ns string) (string, error) {
	if strings.ToLower(kind) != "node" {
		if err := c.checkNS(ns); err != nil {
			return "", err
		}
	}
	switch strings.ToLower(kind) {
	case "pod":
		return c.describePod(ctx, name, ns)
	case "deployment", "deploy":
		return c.describeDeployment(ctx, name, ns)
	case "node":
		return c.describeNode(ctx, name)
	case "persistentvolumeclaim", "pvc":
		return c.describePVC(ctx, name, ns)
	default:
		return "", fmt.Errorf("unsupported kind: %s", kind)
	}
}

// GetLogs fetches the last N log lines from a pod, checking previous containers if needed.
func (c *Client) GetLogs(ctx context.Context, pod, ns string, lines int64) ([]string, error) {
	if err := c.checkNS(ns); err != nil {
		return nil, err
	}

	tryGetLogs := func(previous bool) ([]string, error) {
		req := c.cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
			TailLines: &lines,
			Previous:  previous,
		})
		stream, err := req.Stream(ctx)
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		data, err := io.ReadAll(stream)
		if err != nil {
			return nil, err
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

	result, err := tryGetLogs(false)
	if err != nil || len(result) == 0 {
		prev, prevErr := tryGetLogs(true)
		if prevErr == nil && len(prev) > 0 {
			return prev, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("stream logs: %w", err)
	}
	return result, nil
}

// GetEvents retrieves recent Warning events for a specific namespace.
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

// ListRelated finds pods and deployments related to a specific service or application name.
func (c *Client) ListRelated(ctx context.Context, service, ns string) (string, error) {
	if err := c.checkNS(ns); err != nil {
		return "", err
	}

	var sb strings.Builder

	// 1. Try to find the Service.
	svc, err := c.cs.CoreV1().Services(ns).Get(ctx, service, metav1.GetOptions{})
	if err == nil && len(svc.Spec.Selector) > 0 {
		fmt.Fprintf(&sb, "Service %s/%s found\n", ns, service)
		fmt.Fprintf(&sb, "  ClusterIP: %s\n", svc.Spec.ClusterIP)
		fmt.Fprintf(&sb, "  Ports: ")
		for _, p := range svc.Spec.Ports {
			fmt.Fprintf(&sb, "%d/%s ", p.Port, p.Protocol)
		}
		fmt.Fprintf(&sb, "\n")

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
		return sb.String(), nil
	}

	// 2. Try to find the Deployment.
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

	// 3. Fallback to finding pods by app=<name> label.
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

// ListPodsByNode returns all pods running on a specific node.
func (c *Client) ListPodsByNode(ctx context.Context, node string) (string, error) {
	pods, err := c.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node,
	})
	if err != nil {
		return "", fmt.Errorf("list pods by node: %w", err)
	}

	if len(pods.Items) == 0 {
		return fmt.Sprintf("No pods found on node %q", node), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Pods on node %s (%d total):\n", node, len(pods.Items))
	for _, p := range pods.Items {
		fmt.Fprintf(&sb, "  - %s/%s phase=%s restarts=%d\n",
			p.Namespace, p.Name, p.Status.Phase, totalRestarts(p))
	}
	return sb.String(), nil
}

// GetResourceYAML returns a cleaned YAML representation of a resource.
func (c *Client) GetResourceYAML(ctx context.Context, kind, name, ns string) (string, error) {
	if strings.ToLower(kind) != "node" {
		if err := c.checkNS(ns); err != nil {
			return "", err
		}
	}

	var obj runtime.Object
	var err error

	switch strings.ToLower(kind) {
	case "pod":
		obj, err = c.cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	case "deployment", "deploy":
		obj, err = c.cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	case "statefulset", "sts":
		obj, err = c.cs.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
	case "daemonset", "ds":
		obj, err = c.cs.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
	case "node":
		obj, err = c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	case "persistentvolumeclaim", "pvc":
		obj, err = c.cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	case "configmap", "cm":
		obj, err = c.cs.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	case "service", "svc":
		obj, err = c.cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	default:
		return "", fmt.Errorf("unsupported kind for get_resource_yaml: %s", kind)
	}
	if err != nil {
		return "", fmt.Errorf("get %s/%s: %w", kind, name, err)
	}

	return marshalCleanYAML(obj)
}

// GetHPA analyzes a HorizontalPodAutoscaler's status and metrics.
func (c *Client) GetHPA(ctx context.Context, name, ns string) (string, error) {
	if err := c.checkNS(ns); err != nil {
		return "", err
	}

	hpa, err := c.cs.AutoscalingV2().HorizontalPodAutoscalers(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// Try falling back to finding the HPA by its target reference name.
		list, listErr := c.cs.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
		if listErr != nil {
			return "", fmt.Errorf("get hpa: %w", err)
		}
		for _, h := range list.Items {
			if h.Spec.ScaleTargetRef.Name == name {
				hpa = &h
				break
			}
		}
		if hpa == nil {
			return fmt.Sprintf("No HPA found for %q in namespace %q", name, ns), nil
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "HPA: %s/%s\n", ns, hpa.Name)
	fmt.Fprintf(&sb, "Target: %s/%s\n", hpa.Spec.ScaleTargetRef.Kind, hpa.Spec.ScaleTargetRef.Name)
	fmt.Fprintf(&sb, "Replicas: current=%d desired=%d min=%d max=%d\n",
		hpa.Status.CurrentReplicas,
		hpa.Status.DesiredReplicas,
		*hpa.Spec.MinReplicas,
		hpa.Spec.MaxReplicas,
	)

	// Compare current values vs configured targets.
	if len(hpa.Spec.Metrics) > 0 {
		fmt.Fprintf(&sb, "\nMetrics:\n")
		for i, m := range hpa.Spec.Metrics {
			var currentStr string
			if i < len(hpa.Status.CurrentMetrics) {
				currentStr = formatCurrentMetric(hpa.Status.CurrentMetrics[i])
			}

			switch m.Type {
			case autoscalingv2.ResourceMetricSourceType:
				target := m.Resource.Target
				fmt.Fprintf(&sb, "  resource/%s: target=%s%s%s\n",
					m.Resource.Name,
					formatMetricTarget(target),
					optStr(" current=", currentStr),
					"",
				)
			case autoscalingv2.PodsMetricSourceType:
				fmt.Fprintf(&sb, "  pods/%s: target=%s%s\n",
					m.Pods.Metric.Name,
					formatMetricTarget(m.Pods.Target),
					optStr(" current=", currentStr),
				)
			case autoscalingv2.ObjectMetricSourceType:
				fmt.Fprintf(&sb, "  object/%s: target=%s%s\n",
					m.Object.Metric.Name,
					formatMetricTarget(m.Object.Target),
					optStr(" current=", currentStr),
				)
			case autoscalingv2.ExternalMetricSourceType:
				fmt.Fprintf(&sb, "  external/%s: target=%s%s\n",
					m.External.Metric.Name,
					formatMetricTarget(m.External.Target),
					optStr(" current=", currentStr),
				)
			}
		}
	}

	// Provide visibility into potential blockages via conditions.
	if len(hpa.Status.Conditions) > 0 {
		fmt.Fprintf(&sb, "\nConditions:\n")
		for _, cond := range hpa.Status.Conditions {
			fmt.Fprintf(&sb, "  %s=%s reason=%s\n", cond.Type, cond.Status, cond.Reason)
			if cond.Message != "" {
				fmt.Fprintf(&sb, "    message: %s\n", cond.Message)
			}
		}
	}

	// Highlight obvious scaling issues.
	if hpa.Status.CurrentReplicas == hpa.Spec.MaxReplicas {
		fmt.Fprintf(&sb, "\nWARNING: at max replicas (%d) — scaling is blocked\n", hpa.Spec.MaxReplicas)
	}
	if hpa.Status.DesiredReplicas > hpa.Status.CurrentReplicas {
		fmt.Fprintf(&sb, "\nWARNING: desired=%d > current=%d — scale-up in progress or blocked\n",
			hpa.Status.DesiredReplicas, hpa.Status.CurrentReplicas)
	}

	return sb.String(), nil
}

// ── private ───────────────────────────────────────────────────────────────────

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

		// Check all three possible container states.
		switch {
		case cs.State.Running != nil:
			fmt.Fprintf(&sb, "  State: Running startedAt=%s\n",
				cs.State.Running.StartedAt.Format("2006-01-02T15:04:05"))
		case cs.State.Terminated != nil:
			t := cs.State.Terminated
			fmt.Fprintf(&sb, "  State: Terminated reason=%s exitCode=%d\n", t.Reason, t.ExitCode)
			if t.Message != "" {
				fmt.Fprintf(&sb, "    message: %s\n", t.Message)
			}
		case cs.State.Waiting != nil:
			fmt.Fprintf(&sb, "  State: Waiting reason=%s\n", cs.State.Waiting.Reason)
			if cs.State.Waiting.Message != "" {
				fmt.Fprintf(&sb, "    message: %s\n", cs.State.Waiting.Message)
			}
		}

		// Show the previous state to help debug CrashLoop events.
		if t := cs.LastTerminationState.Terminated; t != nil {
			fmt.Fprintf(&sb, "  LastTermination: reason=%s exitCode=%d\n", t.Reason, t.ExitCode)
			if t.Message != "" {
				fmt.Fprintf(&sb, "    message: %s\n", t.Message)
			}
		}
	}

	for _, c := range pod.Spec.Containers {
		fmt.Fprintf(&sb, "\nResources[%s]:\n", c.Name)
		fmt.Fprintf(&sb, "  limits:   cpu=%s memory=%s\n",
			c.Resources.Limits.Cpu(), c.Resources.Limits.Memory())
		fmt.Fprintf(&sb, "  requests: cpu=%s memory=%s\n",
			c.Resources.Requests.Cpu(), c.Resources.Requests.Memory())

		// Extract and format environment variables.
		if len(c.Env) > 0 {
			fmt.Fprintf(&sb, "  env:\n")
			for _, e := range c.Env {
				if e.ValueFrom != nil {
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
					} else if e.ValueFrom.FieldRef != nil {
						fmt.Fprintf(&sb, "    %s=<fieldRef:%s>\n",
							e.Name, e.ValueFrom.FieldRef.FieldPath)
					} else if e.ValueFrom.ResourceFieldRef != nil {
						fmt.Fprintf(&sb, "    %s=<resourceRef:%s>\n",
							e.Name, e.ValueFrom.ResourceFieldRef.Resource)
					}
				} else {
					val := e.Value
					if isSensitiveEnv(e.Name) {
						val = "<masked>"
					}
					fmt.Fprintf(&sb, "    %s=%s\n", e.Name, val)
				}
			}
		}

		if c.LivenessProbe != nil {
			fmt.Fprintf(&sb, "  livenessProbe:\n")
			writeProbe(&sb, c.LivenessProbe)
		}

		if c.ReadinessProbe != nil {
			fmt.Fprintf(&sb, "  readinessProbe:\n")
			writeProbe(&sb, c.ReadinessProbe)
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
		fmt.Fprintf(&sb, "Condition: %s=%s reason=%s\n  message=%s\n",
			cond.Type, cond.Status, cond.Reason, cond.Message)
	}

	for _, cond := range node.Status.Conditions {
		if cond.Type == "Ready" && cond.Status != "True" {
			fmt.Fprintf(&sb, "\nNODE IS NOT READY: status=%s reason=%s\n",
				cond.Status, cond.Reason)
			fmt.Fprintf(&sb, "Detail: %s\n", cond.Message)
		}
	}

	return sb.String(), nil
}

func (c *Client) describePVC(ctx context.Context, name, ns string) (string, error) {
	pvc, err := c.cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get pvc: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "PVC: %s/%s\n", ns, name)
	fmt.Fprintf(&sb, "Status: %s\n", pvc.Status.Phase)
	fmt.Fprintf(&sb, "Capacity: %s\n", pvc.Status.Capacity.Storage())
	fmt.Fprintf(&sb, "AccessModes: %v\n", pvc.Spec.AccessModes)
	if pvc.Spec.VolumeName != "" {
		fmt.Fprintf(&sb, "BoundVolume: %s\n", pvc.Spec.VolumeName)
	}
	fmt.Fprintf(&sb, "StorageClass: %s\n", func() string {
		if pvc.Spec.StorageClassName != nil {
			return *pvc.Spec.StorageClassName
		}
		return "<default>"
	}())

	pods, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err == nil {
		var mountedBy []string
		for _, p := range pods.Items {
			for _, vol := range p.Spec.Volumes {
				if vol.PersistentVolumeClaim != nil &&
					vol.PersistentVolumeClaim.ClaimName == name {
					mountedBy = append(mountedBy,
						fmt.Sprintf("%s (phase=%s, restarts=%d)",
							p.Name, p.Status.Phase, totalRestarts(p)))
				}
			}
		}
		if len(mountedBy) > 0 {
			fmt.Fprintf(&sb, "MountedBy:\n")
			for _, m := range mountedBy {
				fmt.Fprintf(&sb, "  - %s\n", m)
			}
		} else {
			fmt.Fprintf(&sb, "MountedBy: none\n")
		}
	}

	return sb.String(), nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// writeProbe standardizes the output format for a given probe configuration.
func writeProbe(sb *strings.Builder, p *corev1.Probe) {
	if p.Exec != nil {
		fmt.Fprintf(sb, "    exec: %v\n", p.Exec.Command)
	}
	if p.HTTPGet != nil {
		fmt.Fprintf(sb, "    httpGet: %s:%d%s\n",
			p.HTTPGet.Host,
			p.HTTPGet.Port.IntValue(),
			p.HTTPGet.Path,
		)
	}
	if p.TCPSocket != nil {
		fmt.Fprintf(sb, "    tcpSocket: %s:%s\n",
			p.TCPSocket.Host,
			p.TCPSocket.Port.String(),
		)
	}
	fmt.Fprintf(sb, "    failureThreshold=%d periodSeconds=%d initialDelaySeconds=%d\n",
		p.FailureThreshold, p.PeriodSeconds, p.InitialDelaySeconds)
}

// marshalCleanYAML serializes an object into YAML, omitting irrelevant noisy fields.
func marshalCleanYAML(obj runtime.Object) (string, error) {
	// Set GroupVersionKind if it is missing.
	gvks, _, err := scheme.Scheme.ObjectKinds(obj)
	if err == nil && len(gvks) > 0 {
		obj.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{
			Group:   gvks[0].Group,
			Version: gvks[0].Version,
			Kind:    gvks[0].Kind,
		})
	}

	// Marshal to generic map to easily remove fields.
	data, err := yaml.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("marshal yaml: %w", err)
	}

	// Unmarshal back to a map and clean it.
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("unmarshal for cleaning: %w", err)
	}

	cleanObject(raw)

	cleaned, err := yaml.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("marshal cleaned yaml: %w", err)
	}

	result := string(cleaned)
	// Truncate output to avoid exceeding context budget limits.
	const maxBytes = 8000
	if len(result) > maxBytes {
		result = result[:maxBytes] + "\n# [truncated — use describe_resource for key fields]\n"
	}
	return result, nil
}

// cleanObject recursively removes noisy or irrelevant fields from the object.
func cleanObject(obj map[string]any) {
	// Always remove these noisy top-level fields.
	noisy := []string{
		"managedFields",
		"generation",
		"resourceVersion",
		"uid",
		"creationTimestamp",
		"selfLink",
	}
	for _, f := range noisy {
		delete(obj, f)
	}

	// Clean up metadata to only keep essential identifiers.
	if meta, ok := obj["metadata"].(map[string]any); ok {
		keep := map[string]bool{"name": true, "namespace": true, "labels": true, "annotations": true}
		for k := range meta {
			if !keep[k] {
				delete(meta, k)
			}
		}
	}

	// Drop entire status since describe_resource handles it explicitly.
	delete(obj, "status")

	for _, v := range obj {
		switch vv := v.(type) {
		case map[string]any:
			cleanObject(vv)
		case []any:
			for _, item := range vv {
				if m, ok := item.(map[string]any); ok {
					cleanObject(m)
				}
			}
		}
	}
}

// formatMetricTarget formats the target value of an HPA metric.
func formatMetricTarget(t autoscalingv2.MetricTarget) string {
	switch t.Type {
	case autoscalingv2.UtilizationMetricType:
		if t.AverageUtilization != nil {
			return fmt.Sprintf("%d%%", *t.AverageUtilization)
		}
	case autoscalingv2.AverageValueMetricType:
		if t.AverageValue != nil {
			return t.AverageValue.String()
		}
	case autoscalingv2.ValueMetricType:
		if t.Value != nil {
			return t.Value.String()
		}
	}
	return "unknown"
}

// formatCurrentMetric formats the current value of an HPA metric.
func formatCurrentMetric(m autoscalingv2.MetricStatus) string {
	switch m.Type {
	case autoscalingv2.ResourceMetricSourceType:
		if m.Resource != nil && m.Resource.Current.AverageUtilization != nil {
			return fmt.Sprintf("%d%%", *m.Resource.Current.AverageUtilization)
		}
		if m.Resource != nil && m.Resource.Current.AverageValue != nil {
			return m.Resource.Current.AverageValue.String()
		}
	case autoscalingv2.PodsMetricSourceType:
		if m.Pods != nil {
			return m.Pods.Current.AverageValue.String()
		}
	case autoscalingv2.ObjectMetricSourceType:
		if m.Object != nil {
			return m.Object.Current.Value.String()
		}
	case autoscalingv2.ExternalMetricSourceType:
		if m.External != nil && m.External.Current.AverageValue != nil {
			return m.External.Current.AverageValue.String()
		}
	}
	return ""
}

func optStr(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

// isSensitiveEnv masks environment variables containing typical credential names.
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

// labelMapToSelector converts a key-value label map into a comma-separated selector string.
func labelMapToSelector(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

// totalRestarts aggregates the restart count across all containers in a pod.
func totalRestarts(pod corev1.Pod) int32 {
	var total int32
	for _, cs := range pod.Status.ContainerStatuses {
		total += cs.RestartCount
	}
	return total
}