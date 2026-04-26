# AI SRE Agent

AI-powered SRE агент для автоматического расследования инцидентов в Kubernetes кластере.

## Как работает

1. Alertmanager шлёт webhook при срабатывании алерта
2. Агент запускает ReAct loop: собирает данные через инструменты (describe pod, logs, events, runbook)
3. Локальная LLM (Ollama / Qwen2.5-7B) анализирует собранные данные
4. Агент отправляет отчёт с root cause и рекомендациями в Telegram / stdout

## Быстрый старт

```bash
# Локальная разработка
export KUBECONFIG=~/.kube/config
export NAMESPACES=default,production
export OLLAMA_URL=http://localhost:11434
export NOTIFIER_TYPE=stdout

make run
```

## Установка в кластер

```bash
helm install ai-sre ./helm/ai-sre \
  --namespace ai-sre \
  --create-namespace \
  --set namespaces="{production,staging}" \
  --set notifier.type=telegram \
  --set-string ollama.model=qwen2.5:7b
```

Создать secret для Telegram:
```bash
kubectl create secret generic ai-sre-secrets \
  --namespace ai-sre \
  --from-literal=telegram-token=YOUR_TOKEN \
  --from-literal=telegram-chat-id=YOUR_CHAT_ID
```

## Настройка Alertmanager

```yaml
# alertmanager.yaml
route:
  group_by: ['alertname', 'namespace']
  group_wait: 30s
  group_interval: 5m
  receiver: ai-sre

receivers:
  - name: ai-sre
    webhook_configs:
      - url: http://ai-sre.ai-sre.svc.cluster.local:8080/webhook
```

## Тестирование

```bash
# Unit тесты
make test

# Integration тесты с kind
make kind-setup
make test-integration
make kind-teardown
```

## Конфигурация

Все параметры настраиваются через `helm/ai-sre/values.yaml`.
Переменные окружения для локальной разработки — см. `internal/config/config.go`.

## Архитектура

```
Alertmanager → webhook → Agent → ReAct loop → Ollama (LLM)
                                     ↓
                              K8s API (describe, logs, events)
                                     ↓
                              Notifier (Telegram / stdout)
```

## TODO

См. [TODO.md](TODO.md)
