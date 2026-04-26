# SREngine — TODO

Документ фиксирует всё что обсуждалось в архитектурной сессии.
Цель: спустя время было понятно что сделано, что нет и почему.

---

## Сделано

### ReAct loop — Agent struct
- Agent struct с полями: llm, tools, notifier, cfg, logger
- Investigate(ctx, alert) — единственная точка входа
- ReAct loop: Think → Act → Observe → повтор до Answer или maxSteps
- maxSteps = 8, конфигурируемо
- При достижении maxSteps — частичный отчёт с confidence=low

### ToolRegistry + инструменты
- Интерфейс Tool (Name, Description, Execute)
- ToolRegistry с методом Execute(ctx, ToolCall)
- describe_resource(kind, name, namespace) — pod / deploy / node / pvc
- get_logs(name, namespace) — с дедупликацией
- get_events(namespace) — только Warning events
- fetch_runbook(url) — HTTP GET, trim до 2000 символов
- list_related(service, namespace) — поиск по labels / ownerReferences
- list_pods_by_node(node) — для node-level алертов
- get_resource_yaml(kind, name, namespace)
- get_hpa(name, namespace)
- check_metrics(promql, range_minutes, limit_promql?) — Prometheus HTTP API

### K8s клиент (client-go)
- NewK8sClient — InClusterConfig с fallback на KUBECONFIG
- describePod, describeDeployment, describeNode, describePVC
- GetLogs — последние N строк, передача в дедупликатор
- GetEvents, ListRelated, ListPodsByNode, GetResourceYAML, GetHPA

### Log deduplication
- normalize(line) — strip timestamp, IP, UUID, hex, числа → плейсхолдеры
- DeduplicateLogs(lines) — Drain-подобный алгоритм
- LogPattern: Template, Count, First, Last, Sample, Level
- Фильтрация: только ERROR и WARN, не более 20 паттернов

### Alertmanager webhook
- HTTP сервер на порту 8080
- POST /webhook — принимает Alertmanager payload
- Дедупликация алертов по fingerprint
- Очередь: последовательная обработка
- Таймаут на расследование: 5 минут

### Notifier
- StdoutNotifier (fallback)
- TelegramNotifier
- EmailNotifier (HTML-шаблон)
- WebhookNotifier (generic JSON POST)
- MultiNotifier — параллельная рассылка во все каналы

### Промпт
- System prompt с ролью, инструментами, правилами, форматом JSON
- Alert context — отдельное user-сообщение
- tool_result — роль user с JSON-обёрткой

### Отчёт
- Report: Summary, RootCause, Confidence, Actions[], SkippedNamespaces[], StepsUsed, Duration
- Action: Priority, Description, Command, RiskLevel

### Helm chart
- deployment, service, configmap (промпт), rbac (Role + RoleBinding per namespace), values.yaml

### Тестирование
- kind кластер (2 nodes)
- Сценарии: test-oom, test-crashloop, test-cascade, test-quota, test-liveness, test-pv, test-node-notready, test-high-memory
- Все сценарии PASS

---

## В работе / Приоритет 1

### CI (GitHub Actions)
- [ ] kind кластер через `helm/kind-action`
- [ ] Установка kube-prometheus-stack (prometheus + alertmanager, без Grafana)
- [ ] Деплой тестовых сценариев
- [ ] sleep после деплоя — даём подам упасть и войти в CrashLoop
- [ ] `go test ./tests/integration/... -v -timeout=300s`
- [ ] Использовать внешний LLM API в CI (Ollama не запустить в GitHub Actions)

### AI.md
- [ ] Документ с правилами и анти-паттернами для AI агента

---

## TODO (отложено)

### Внешние LLM провайдеры
- [ ] AnthropicProvider (Claude API)
- [ ] OpenAIProvider (GPT-4o)
- [ ] Логика выбора: Ollama → Anthropic → OpenAI
- [ ] Конфигурация через values.yaml: llm.provider, llm.fallback
- [ ] API ключи через Kubernetes Secret

### Human Approval Gate
- [ ] risk_score по эвристикам (production NS? stateful? DB?)
- [ ] Slack кнопки Approve / Reject для medium risk
- [ ] Только отчёт без действий для high risk

### Метрики агента
- [ ] alerts_processed, steps_per_investigation, confidence_distribution, investigation_duration_seconds
- [ ] ServiceMonitor для scrape агента

### Helm chart production-ready
- [ ] PodDisruptionBudget
- [ ] NetworkPolicy
- [ ] Resource limits для агента
- [ ] Liveness / Readiness probes

---

## Решения принятые в архитектуре

| Решение | Выбор | Причина |
|---|---|---|
| Язык | Go | Скорость, низкое потребление памяти |
| Первый LLM провайдер | Ollama local | CPU-only, нет GPU в кластерах |
| Модель | Qwen2.5-7B Q4 | Лучший tool-use на CPU, 5GB RAM |
| RBAC | Role per namespace | Не ClusterRole, минимальные права |
| Log deduplication | Drain-подобный алгоритм | Динамические паттерны, не захардкожены |
| Автовыполнение | Отключено | Только рекомендации |
| Формат ответа модели | JSON в system prompt | Работает на любой модели, не только с нативным tool-use |
| Тестирование | kind + integration tests | Реальный кластер, предсказуемые сценарии |
| Детекторы в коде | Хардкод в runReAct | Модель на CPU ненадёжно следует промпту, детекторы надёжнее |