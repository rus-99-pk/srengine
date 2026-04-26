# AI SRE Agent — TODO

Документ фиксирует всё что обсуждалось в архитектурной сессии.
Цель: спустя время было понятно что сделано, что нет и почему.

---

## В работе / Приоритет 1

### Ollama интеграция
Первый провайдер LLM. Запускается как отдельный под в кластере.
Модель: `qwen2.5:7b-instruct-q4_K_M` — лучший баланс quality/size для CPU-only.
RAM: ~5GB. Работает без GPU.

- [ ] Реализовать `OllamaProvider` — POST `/api/chat` с messages[]
- [ ] Настроить keep-alive чтобы модель не выгружалась между запросами
- [ ] Добавить retry + timeout (модель на CPU отвечает медленно, ~30-60s)
- [ ] Проверить что Qwen2.5-7B корректно возвращает JSON без markdown-обёртки
- [ ] Добавить валидацию ответа: если не JSON — повторить запрос (max 2 retry)

---

### ReAct loop — Agent struct
Центральная логика агента.

- [ ] Реализовать `Agent` struct с полями: k8s, llm, tools, cfg, logger
- [ ] Реализовать `Investigate(ctx, alert) (*Report, error)` — единственная точка входа
- [ ] Реализовать ReAct loop: Think → Act → Observe → повтор до Answer или maxSteps
- [ ] Ограничение: maxSteps = 8 (конфигурируемо через values.yaml)
- [ ] При достижении maxSteps — вернуть частичный отчёт с confidence=low, не падать

---

### ToolRegistry + инструменты
- [ ] Реализовать интерфейс `Tool` (Name, Description, Execute)
- [ ] Реализовать `ToolRegistry` с методом `Execute(ctx, ToolCall)`
- [ ] Инструменты для реализации:
  - [ ] `describe_resource(kind, name, namespace)` — pod / deploy / node
  - [ ] `get_logs(name, namespace, lines, level)` — с дедупликацией перед отдачей
  - [ ] `get_events(namespace, since_minutes)` — только Warning events
  - [ ] `fetch_runbook(url)` — HTTP GET, trim до 2000 символов
  - [ ] `list_related(service, namespace)` — поиск по labels / ownerReferences
  - [ ] `check_metrics(promql, range_minutes)` — запрос в Prometheus HTTP API

---

### K8s клиент (client-go)
- [ ] `NewK8sClient` — InClusterConfig с fallback на KUBECONFIG (для локальной разработки)
- [ ] `CheckAccess(ctx)` — проверка доступа к каждому NS при старте, результат в лог и отчёт
- [ ] `describePod` — форматированный вывод без сырого YAML (только нужные поля)
- [ ] `describeDeployment` — replicas, strategy, conditions
- [ ] `describeNode` — capacity, allocatable, conditions
- [ ] `GetLogs` — последние N строк, фильтр по уровню, передать в дедупликатор

---

### Log deduplication
Алгоритм на основе Drain. Паттерны выводятся динамически из самих логов — не захардкожены.

- [ ] Реализовать `normalize(line string) string`:
  - strip: timestamp, IP, UUID, hex, path, quoted strings, числа → плейсхолдеры
- [ ] Реализовать `DeduplicateLogs(lines []string) []LogPattern`
- [ ] `LogPattern`: Template, Count, First, Last, Sample (одна оригинальная строка), Level
- [ ] Сортировка по Count desc — самые частые первыми
- [ ] Фильтрация перед дедупликацией: в модель идут только ERROR и WARN
  - INFO / DEBUG отбрасываются если нет явной причины их смотреть
- [ ] Ограничение на выход: не более 20 уникальных паттернов в модель

---

### RBAC
Не ClusterRole — набор Role per namespace.

- [ ] В values.yaml: `rbac.namespaces: [production, staging, monitoring]`
- [ ] Helm генерирует Role + RoleBinding для каждого namespace из списка
- [ ] Права: get/list/watch на pods, deployments, events, logs (read-only)
- [ ] При старте агента: `CheckAccess` пишет в лог какие NS доступны, какие нет
- [ ] В финальном отчёте: поле `skipped_namespaces` со списком и причиной

---

### Alertmanager webhook
- [ ] HTTP сервер на порту 8080 (конфигурируемо)
- [ ] `POST /webhook` — принимает Alertmanager payload
- [ ] Дедупликация алертов: один алерт не запускает два расследования одновременно
- [ ] Очередь: если пришло 5 алертов сразу — обрабатываем последовательно
- [ ] Таймаут на расследование: 5 минут (конфигурируемо)

---

### Промпт
- [ ] System prompt: роль, список инструментов с сигнатурами, правила, формат JSON
- [ ] Правила в промпте (явно, модель сама не выведет):
  - сначала describe_resource, потом get_logs
  - если в логах упоминается другой сервис — расследовать его тоже
  - не выдумывать данные
  - список доступных namespaces прямо в промпте
- [ ] Alert context: отдельное user-сообщение с алертом, labels, runbook URL
- [ ] tool_result: роль `user` с JSON-обёрткой `{"tool":"...","result":"..."}`
- [ ] Context budget: system ~400 tok, alert ~300 tok, ReAct turns ~2500 tok — итого ~3200 из 4096

---

### Отчёт
- [ ] Структура `Report`: Summary, RootCause, Confidence (high/medium/low),
  Actions[], SkippedNamespaces[], StepsUsed, Duration
- [ ] `Action`: Priority, Description, Command (опционально), RiskLevel (low/medium/high)
- [ ] Отправка в Slack (webhook) — форматированное сообщение с полями отчёта
- [ ] Нет автовыполнения действий — только рекомендации

---

## Структура проекта

```
ai-sre/
├── cmd/agent/              — main, флаги, запуск HTTP сервера
├── internal/
│   ├── agent/              — Agent struct, Investigate, ReAct loop
│   ├── tools/              — ToolRegistry, Tool interface, все реализации
│   ├── k8s/                — K8sClient, describe*, getLogs, getEvents
│   ├── llm/                — LLMProvider interface, OllamaProvider
│   ├── logs/               — DeduplicateLogs, normalize, LogPattern
│   └── alert/              — Alert struct, webhook handler, очередь
├── tests/
│   ├── integration/        — сценарии с kind кластером
│   └── scenarios/          — yaml манифесты проблемных подов
└── helm/ai-sre/
    ├── templates/
    │   ├── deployment.yaml
    │   ├── configmap.yaml  — промпт, конфиг
    │   ├── rbac.yaml       — Role + RoleBinding per namespace
    │   └── service.yaml
    └── values.yaml
```

---

## Тестирование

### kind кластер
- [ ] Настроить kind config (минимум 2 nodes)
- [ ] Установить kube-prometheus-stack (только prometheus + alertmanager, без Grafana)
  - Grafana не нужна для тестов агента, экономим ресурсы
- [ ] Тестовые сценарии (каждый в отдельном namespace):
  - [ ] `test-oom` — под с memory limit меньше потребления (polinux/stress)
  - [ ] `test-crashloop` — под падает с exit 1, пишет в лог имя недоступного сервиса
  - [ ] `test-cascade` — frontend + backend, backend падает → cascade ошибки во frontend
  - [ ] `test-quota` — namespace с ResourceQuota, под не может задеплоиться

### Go integration tests
- [ ] `Scenario` struct: AlertFixture, ExpectIn[], ExpectActions[], MaxSteps
- [ ] Assertions: root_cause содержит ключевые слова, actions содержат хотя бы одно из ожидаемых
- [ ] Логировать: StepsUsed, Confidence, RootCause для каждого сценария
- [ ] Алерты подаются напрямую через AlertFixture — не ждём Alertmanager

### CI (GitHub Actions)
- [ ] kind кластер через `helm/kind-action`
- [ ] Установка тестовых сценариев
- [ ] sleep 30 после деплоя — даём подам упасть и войти в CrashLoop
- [ ] `go test ./tests/integration/... -v -timeout=300s`
- [ ] В CI использовать внешний LLM API (см. TODO ниже)

---

## TODO (отложено)

### Внешние LLM провайдеры
Сейчас используем только Ollama (local). В будущем нужен fallback если Ollama недоступна,
и возможность использовать более мощную модель для сложных кейсов.

- [ ] Реализовать интерфейс `LLMProvider`:
  ```go
  type LLMProvider interface {
      Complete(ctx context.Context, messages []Message) (string, error)
      Name() string
  }
  ```
- [ ] Реализовать `AnthropicProvider` (Claude API)
- [ ] Реализовать `OpenAIProvider` (GPT-4o)
- [ ] Логика выбора провайдера: Ollama → Anthropic → OpenAI (по приоритету)
- [ ] Конфигурация через values.yaml: `llm.provider: ollama`, `llm.fallback: anthropic`
- [ ] API ключи через Kubernetes Secret, не в values.yaml

### Human Approval Gate
Автовыполнение не нужно на первом этапе. Но в будущем:
- [ ] risk_score по эвристикам (production NS? stateful? DB?)
- [ ] Slack кнопки Approve / Reject для medium risk
- [ ] Только отчёт без действий для high risk

### Метрики агента
- [ ] Prometheus метрики самого агента: alerts_processed, steps_per_investigation,
  confidence_distribution, investigation_duration_seconds
- [ ] ServiceMonitor для scrape агента

### Расширенные инструменты
- [ ] `get_resource_yaml(kind, name, namespace)` — для сложных кейсов когда нужен полный spec
- [ ] `list_pods_by_node(node)` — когда алерт на уровне ноды
- [ ] `get_hpa(name, namespace)` — анализ HorizontalPodAutoscaler

### Helm chart production-ready
- [ ] PodDisruptionBudget
- [ ] NetworkPolicy (агент должен ходить только в K8s API и Ollama)
- [ ] Resource limits для самого агента
- [ ] Liveness / Readiness probes

---

## Решения принятые в архитектуре

Зафиксировано чтобы не переобсуждать.

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
