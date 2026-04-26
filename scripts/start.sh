#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
KIND_CONFIG="$SCRIPT_DIR/kind-config.yaml"
CLUSTER_NAME="srengine-dev"

# --- Режимы запуска ---
MODE="${1:-local}"  # local | kind | kind-reset

log() { echo "[start.sh] $*"; }

# ========================
# kind кластер
# ========================

kind_up() {
	if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
		log "kind cluster '$CLUSTER_NAME' already exists, skipping create"
	else
		log "creating kind cluster '$CLUSTER_NAME'..."
		kind create cluster --config "$KIND_CONFIG" --name "$CLUSTER_NAME"
	fi

	log "switching kubectl context to kind-${CLUSTER_NAME}"
	kubectl cluster-info --context "kind-${CLUSTER_NAME}" > /dev/null
	kubectl config use-context "kind-${CLUSTER_NAME}"
}

# ========================
# Prometheus
# ========================

install_prometheus() {
	log "installing kube-prometheus-stack..."

	# Добавляем helm репо если нет
	if ! helm repo list 2>/dev/null | grep -q "prometheus-community"; then
		helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
	fi
	helm repo update

	# Устанавливаем только prometheus + alertmanager, без grafana — экономим ресурсы
	helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
		--namespace monitoring \
		--create-namespace \
		--set grafana.enabled=false \
		--set prometheus.prometheusSpec.scrapeInterval=15s \
		--set prometheus.prometheusSpec.evaluationInterval=15s \
		--set prometheus.prometheusSpec.retention=1h \
		--set prometheus.prometheusSpec.resources.requests.memory=512Mi \
		--set prometheus.prometheusSpec.resources.limits.memory=1Gi \
		--set alertmanager.enabled=false \
		--set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
		--set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
		--set-json 'prometheus.prometheusSpec.podMonitorNamespaceSelector={}' \
		--set-json 'prometheus.prometheusSpec.serviceMonitorNamespaceSelector={}' \
		--set-json 'prometheus.prometheusSpec.ruleNamespaceSelector={}' \
		--wait \
		--timeout=5m

	log "prometheus installed, waiting for pods..."
	kubectl wait --for=condition=ready pod \
		-l app.kubernetes.io/name=prometheus \
		-n monitoring \
		--timeout=120s

	PROM_URL="http://$(kubectl get svc -n monitoring -l app=kube-prometheus-stack-prometheus -o jsonpath='{.items[0].spec.clusterIP}'):9090"
	log "prometheus URL: $PROM_URL"
	export PROMETHEUS_URL="$PROM_URL"
}

kind_down() {
	if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
		log "deleting kind cluster '$CLUSTER_NAME'..."
		kind delete cluster --name "$CLUSTER_NAME"
	else
		log "cluster '$CLUSTER_NAME' not found, nothing to delete"
	fi
}

# ========================
# Namespaces для тестов
# ========================

TEST_NAMESPACES=(
	test-cascade
	test-oom
	test-crashloop
	test-quota
	test-node-notready
	test-liveness
	test-pv
	test-high-memory
)

ensure_namespaces() {
	log "ensuring test namespaces exist..."
	for ns in "${TEST_NAMESPACES[@]}"; do
		kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
	done
}

# ========================
# Port-forward для Prometheus
# ========================

start_prometheus_portforward() {
	log "starting prometheus port-forward on localhost:9090..."
	# Убиваем предыдущий port-forward если был
	pkill -f "port-forward.*prometheus" 2>/dev/null || true
	kubectl port-forward -n monitoring \
		svc/kube-prometheus-stack-prometheus 9090:9090 &>/dev/null &
	PF_PID=$!
	sleep 2
	if kill -0 $PF_PID 2>/dev/null; then
		log "port-forward running (pid=$PF_PID)"
		export PROMETHEUS_URL="http://localhost:9090"
	else
		log "WARN: port-forward failed, PROMETHEUS_URL will be empty"
	fi
}

# ========================
# Агент
# ========================

run_agent() {
	log "starting agent..."

	export PROMPT_PATH="$ROOT_DIR/internal/agent/prompt.txt"
	export NAMESPACES="default,$(IFS=,; echo "${TEST_NAMESPACES[*]}")"
	export OLLAMA_URL="${OLLAMA_URL:-http://localhost:11434}"
	export OLLAMA_MODEL="${OLLAMA_MODEL:-qwen2.5:3b}"
	export NOTIFIER_TYPE="${NOTIFIER_TYPE:-stdout}"
	export PROMETHEUS_URL="${PROMETHEUS_URL:-}"
	export AGENT_SUMMARIZE_EVERY="${AGENT_SUMMARIZE_EVERY:-0}"
    export AGENT_MAX_STEPS=8

	log "NAMESPACES=$NAMESPACES"
	log "OLLAMA_URL=$OLLAMA_URL"
	log "OLLAMA_MODEL=$OLLAMA_MODEL"

	cd "$ROOT_DIR"
	go run ./cmd/agent
}

# ========================
# Точка входа
# ========================

case "$MODE" in
	local)
		log "mode: local (using current kubeconfig)"
		# Если Prometheus уже доступен локально — подключаемся
		if kubectl get svc -n monitoring kube-prometheus-stack-prometheus &>/dev/null 2>&1; then
			start_prometheus_portforward
		fi
		run_agent
		;;

	kind)
		log "mode: kind"
		kind_up
		ensure_namespaces
		run_agent
		;;

	kind-full)
		# Полный стек: kind + Prometheus + агент
		log "mode: kind-full"
		kind_up
		ensure_namespaces
		install_prometheus
		start_prometheus_portforward
		run_agent
		;;

	kind-reset)
		log "mode: kind-reset"
		kind_down
		kind_up
		ensure_namespaces
		run_agent
		;;

	kind-reset-full)
		# Пересоздать всё включая Prometheus
		log "mode: kind-reset-full"
		kind_down
		kind_up
		ensure_namespaces
		install_prometheus
		start_prometheus_portforward
		run_agent
		;;

	kind-down)
		kind_down
		;;

	prometheus)
		# Только установить/переустановить Prometheus в существующий кластер
		log "mode: prometheus"
		install_prometheus
		start_prometheus_portforward
		;;

	*)
		echo "Usage: $0 [local|kind|kind-full|kind-reset|kind-reset-full|kind-down|prometheus]"
		exit 1
		;;
esac
