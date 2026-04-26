# AI.md — SREngine: контекст для AI-ассистента

Этот файл читает AI-ассистент перед началом работы над проектом.
Цель: понять архитектуру, знать где что лежит, не задавать лишних вопросов.

---

## Что такое SREngine

AI-агент для автоматического расследования инцидентов в Kubernetes.
Получает алерт от Alertmanager → запускает ReAct loop → собирает данные через инструменты → 
локальная LLM анализирует → отправляет отчёт с root cause и рекомендациями.

Язык: Go. LLM: Ollama (Qwen2.5-7B, CPU-only). Тестирование: kind + integration tests.

---

## Структура проекта

```
srengine/
├── cmd/agent/main.go           — точка входа, флаги, запуск HTTP сервера
├── internal/
│   ├── agent/
│   │   ├── agent.go            — Agent struct, Investigate(), runReAct(), детекторы
│   │   └── prompt.txt          — system prompt для LLM
│   ├── alert/
│   │   └── server.go           — HTTP /webhook, очередь, дедупликация алертов
│   ├── config/
│   │   └── config.go           — вся конфигурация (env + values.yaml)
│   ├── k8s/
│   │   └── client.go           — K8sClient: describe*, getLogs, getEvents, listRelated
│   ├── llm/
│   │   └── ollama.go           — OllamaProvider: POST /api/chat
│   ├── logs/
│   │   └── dedup.go            — Drain-подобная дедупликация логов
│   ├── notifier/
│   │   └── notifier.go         — Stdout/Telegram/Email/Webhook + MultiNotifier
│   └── tools/
│       └── registry.go         — ToolRegistry, все инструменты
├── tests/
│   ├── integration/
│   │   └── agent_test.go       — end-to-end тесты против реального kind кластера
│   └── scenarios/              — yaml манифесты для каждого тестового сценария
│       ├── test-cascade/
│       ├── test-crashloop/
│       ├── test-high-memory/
│       ├── test-liveness/
│       ├── test-node-notready/
│       ├── test-oom/
│       ├── test-pv/
│       └── test-quota/
├── helm/                       — Helm chart (deployment, rbac, configmap, service)
├── TODO.md                     — что сделано, что в работе, что отложено
└── AI.md                       — этот файл
```

---

## Ключевые файлы — запрашивай если нужны

| Файл | Когда нужен |
|---|---|
| `internal/agent/agent.go` | любые изменения в логике расследования, детекторах, ReAct loop |
| `internal/agent/prompt.txt` | изменения в поведении LLM, правилах, инструментах |
| `internal/tools/registry.go` | добавление/изменение инструментов |
| `internal/k8s/client.go` | изменения в работе с Kubernetes API |
| `internal/llm/ollama.go` | изменения в работе с LLM |
| `internal/logs/dedup.go` | изменения в дедупликации логов |
| `internal/notifier/notifier.go` | изменения в каналах уведомлений |
| `internal/alert/server.go` | изменения в webhook сервере или очереди |
| `internal/config/config.go` | добавление новых параметров конфигурации |
| `tests/integration/agent_test.go` | изменения в тестах |
| `tests/scenarios/*/deployment.yaml` | изменения в тестовых сценариях |

Если проблема неочевидна — запроси файл прежде чем предлагать решение.

---

## Архитектура ReAct loop

```
Alertmanager → POST /webhook → очередь → Investigate()
                                              ↓
                                         runReAct()
                                         ┌─────────────────────────────┐
                                         │ 1. Think: LLM.Complete()    │
                                         │ 2. Parse JSON response      │
                                         │ 3. Answer? → return Report  │
                                         │ 4. Act: Tools.Execute()     │
                                         │ 5. Детекторы (см. ниже)    │
                                         │ 6. Observe: append result   │
                                         │ 7. repeat (max 8 steps)     │
                                         └─────────────────────────────┘
                                              ↓
                                         Notifier.Send(Report)
```

### Формат ответа LLM

Каждый шаг — один JSON объект. Либо action, либо answer:

```json
{"thought":"...","action":{"tool":"tool_name","args":{"key":"value"}}}
{"thought":"...","answer":{"summary":"...","root_cause":"...","confidence":"high|medium|low","actions":[...],"skipped_namespaces":[]}}
```

---

## Детекторы в agent.go — критически важно

Модель Qwen2.5-7B на CPU ненадёжно следует правилам из промпта.
Поэтому в `runReAct()` после каждого tool call стоят хардкод-детекторы
которые анализируют результат и добавляют в контекст принудительные инструкции.

Текущие детекторы (все в `agent.go`):

| Детектор | Триггер | Действие |
|---|---|---|
| `check_metrics` интерпретатор | результат содержит `utilization=` | добавляет hint с severity |
| OOMKilled | `get_logs` вернул пустоту + в истории есть `OOMKilled` | форсирует финальный ответ |
| PodHighMemoryUsage | `get_logs` пустой + alert содержит `HighMemory/MemoryUsage/MemoryPressure` | напрямую вызывает `check_metrics`, минуя модель (модель опечатывает имена) |
| quota exceeded | `get_events` содержит `exceeded quota/FailedCreate/forbidden` | форсирует финальный ответ |
| NotReady нода | `describe_resource` содержит `Kubelet stopped posting node status` | форсирует финальный ответ |
| liveness probe | `describe_resource` содержит `livenessProbe` + `exit 1/exit 2` | форсирует финальный ответ |
| disk full | `get_logs` содержит `disk full/disk usage critical/nearly full/writes failing` | форсирует финальный ответ |
| PVC MountedBy | `describe_resource` содержит `MountedBy:` | форсирует `get_logs` для пода монтирующего PVC |

**Если нужно изменить поведение агента для конкретного сценария — скорее всего менять нужно детектор или промпт, а не логику loop.**

---

## Инструменты (tools/registry.go)

| Инструмент | Сигнатура | Что делает |
|---|---|---|
| `describe_resource` | kind, name, namespace | ключевые поля pod/deploy/node/pvc |
| `get_logs` | name, namespace | дедуплицированные ERROR/WARN паттерны |
| `get_events` | namespace | Warning events из namespace |
| `fetch_runbook` | url | HTTP GET, trim до 2000 символов |
| `list_related` | service, namespace | поиск по labels/ownerReferences |
| `list_pods_by_node` | node | все поды на ноде |
| `get_resource_yaml` | kind, name, namespace | очищенный spec YAML |
| `get_hpa` | name, namespace | HPA состояние |
| `check_metrics` | promql, range_minutes, limit_promql? | Prometheus range query, min/max/avg/last |

---

## Тестовые сценарии

Каждый сценарий — отдельный namespace в kind кластере.
Тест деплоит манифест, ждёт пока поды упадут, шлёт алерт на `/webhook`, ждёт результата.

| Сценарий | Alert | Root cause |
|---|---|---|
| test-cascade | KubePodCrashLooping | frontend → backend → redis (redis не существует) |
| test-oom | KubePodCrashLooping | OOMKilled, memory limit 16Mi |
| test-crashloop | KubePodCrashLooping | missing db-service |
| test-quota | KubeDeploymentReplicasMismatch | ResourceQuota exceeded |
| test-liveness | LivenessProbeFailed | liveness probe команда всегда exit 1 |
| test-pv | KubePersistentVolumeFillingUp | PVC nearly full |
| test-node-notready | KubeNodeNotReady | kubelet stopped (docker pause) |
| test-high-memory | PodHighMemoryUsage | memory > 85% limit (pod Running, нет ошибок) |

---

## Известные особенности модели

- **Опечатывает имена подов** — поэтому детектор PodHighMemoryUsage вызывает `check_metrics` напрямую из Go, не через модель.
- **Игнорирует правила промпта** при большом контексте — детекторы надёжнее промпта.
- **Пытается вызвать `list_related` без service** — защита: инструмент возвращает ошибку, дубль-детектор форсирует финальный ответ.
- **Повторяет одинаковые tool calls** — защита: `seenCalls` map + после 1 дубля подряд форсируется финальный ответ.
- **Медленная** — ~10-15s на шаг на CPU. Таймаут расследования 5 минут.

---

## Конфигурация

Всё через env (локальная разработка) или values.yaml (кластер).
Основные параметры — в `internal/config/config.go`.

Секреты (Telegram token, email password) — через Kubernetes Secret, не в values.yaml.

Детали — в [TODO.md](TODO.md).