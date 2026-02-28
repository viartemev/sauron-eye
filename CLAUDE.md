    # AI Code Reviewer — Architecture & Design Document

> Система автоматического анализа кода на основе Claude API для всех Go-репозиториев в GitLab.
> Находит проблемы в бизнес-логике, архитектуре и системной надёжности через анализ путей выполнения.

---

## Текущая реализация (что уже работает)

Этот репозиторий называется **sauron-eye** (Go module: `sauron-eye`, Go 1.26+).
Реализованы два CLI-инструмента — они являются фундаментом для полной системы, описанной ниже:

### `callgraph` — построитель графов вызовов

Находится в `cmd/callgraph/`. Анализирует Go-проект и выводит call graphs в JSON.

**Стек:**
- `golang.org/x/tools/go/packages` — загрузка пакетов
- `golang.org/x/tools/go/ssa` + `ssautil` — SSA представление
- `golang.org/x/tools/go/callgraph/cha` → `vta` — построение call graph (CHA→VTA, **не PTA**)

**Архитектурные решения, принятые в реализации:**
- Алгоритм **CHA → VTA** (VTA рафинирует CHA, стабилен, без timeout'ов). PTA из документа ниже заменён на VTA.
- `Traversal.BuildNode` использует "sliding window" visited set: добавляем на входе, удаляем при выходе — соседние ветки могут посещать ту же функцию.
- Дополнительный SSA scan при обходе — ловит статические вызовы, которые CHA/VTA пропустил (особенно в сгенерированных клиентах).
- Синтетические `$bound`/`$thunk` обёртки разворачиваются до реального метода в builder'е.
- Исходный код функции извлекается до 150 строк, кешируется построчно.
- `generated: true` выставляется для узлов из auto-generated файлов (`*.pb.go`, `*_gen.go`, `mock_*.go`, и т.д.).
- `pkg-filter` по умолчанию = имя модуля из `go.mod`.

**Детектируемые entry points:**
- HTTP: Gin / Echo / Chi / `net/http`
- Kafka: sarama (Shopify + IBM), confluent-kafka-go, segmentio/kafka-go
- gRPC: `RegisterXxxServer` pattern
- Cron: robfig/cron, go-co-op/gocron

**Метаданные каждого узла:** db_calls, tx_ops, http_calls, kafka_ops, sync_ops, redis_ops, channel_ops.

**Concurrency analysis:** `SharedResourceAnalyzer` строит инвертированный индекс таблиц → entry points, находит конфликты (2+ entry points, минимум 1 WRITE), детектирует защитные механизмы (FOR UPDATE, WHERE version=N, Redis SetNX, atomic UPDATE WHERE).

### `review` — интерактивный AI-ревьюер

Находится в `cmd/review/`. Строит call graph, показывает список entry points, отправляет выбранный граф в Claude.

- `internal/review/claude.go` — прямой HTTP-клиент к `https://api.anthropic.com/v1/messages`, max_tokens=8192. При отсутствии API key — mock (выводит промпт).
- `internal/review/prompt.go` — формирует system prompt + user message с графом, конфликтами и содержимым `checks.md`.
- `checks.md` кладётся в корень **анализируемого проекта** (не этого репо). Если файл не найден — инструмент предупреждает и использует generic prompt.

### Веб-визуализатор

`web/index.html` — single-file визуализатор. Загружает `graph.json`, рисует интерактивный граф.

---

## Содержание

1. [Цели и принципы](#1-цели-и-принципы)
2. [Верхнеуровневая архитектура](#2-верхнеуровневая-архитектура)
3. [Компоненты системы](#3-компоненты-системы)
    - 3.1 [Gateway Service](#31-gateway-service)
    - 3.2 [Job Queue](#32-job-queue)
    - 3.3 [Worker Pool](#33-worker-pool)
    - 3.4 [Call Graph Builder](#34-call-graph-builder)
    - 3.5 [Checker Pipeline](#35-checker-pipeline)
    - 3.6 [Claude API Client](#36-claude-api-client)
    - 3.7 [GitLab Reporter](#37-gitlab-reporter)
    - 3.8 [Config Service](#38-config-service)
4. [Call Graph Builder — детальная архитектура](#4-call-graph-builder--детальная-архитектура)
5. [Checker Pipeline — детальная архитектура](#5-checker-pipeline--детальная-архитектура)
6. [Анализ конкурентных точек входа](#6-анализ-конкурентных-точек-входа)
7. [Схема данных](#7-схема-данных)
8. [Структура репозитория](#8-структура-репозитория)
9. [Деплой и инфраструктура](#9-деплой-и-инфраструктура)
10. [Конфигурация](#10-конфигурация)
11. [Производительность и оптимизации](#11-производительность-и-оптимизации)
12. [Мониторинг](#12-мониторинг)
13. [Roadmap](#13-roadmap)

---

## 1. Цели и принципы

### Что система должна находить

**Системные проблемы**
- Отсутствие транзакций при нескольких DB-операциях
- Транзакция слишком широкая (включает HTTP-вызов)
- HTTP-вызовы без таймаута
- Отсутствие retry и circuit breaker на внешних вызовах
- Kafka publish вне outbox-паттерна (после commit транзакции данные могут не дойти)
- N+1 запросы в циклах

**Проблемы бизнес-логики**
- Race conditions (check-then-act без блокировки)
- Невалидные переходы состояний (state machine нарушения)
- Неверный порядок операций (деньги списаны до резервации товара)
- TOCTOU (Time of Check to Time of Use)
- Отсутствие idempotency при at-least-once доставке Kafka

**Конкурентные проблемы**
- HTTP и Kafka consumer пишут в одни данные без защиты
- Double-spend сценарии
- Lost update при параллельных записях

**Data flow проблемы**
- Unsanitized input попадает в DB запрос
- Поля с деньгами без проверки на отрицательные значения
- IDOR — запрос без фильтрации по владельцу (userID из токена не используется в WHERE)
- Поля с `expires_at` не проверяются перед использованием

**Schema consistency**
- Nullable поля в БД используются как non-pointer в Go
- Запросы без индекса по высоконагруженным таблицам
- Soft delete: запросы без `WHERE deleted_at IS NULL`

### Принципы проектирования

- **Zero config для новых репо** — система подключается через GitLab System Hook один раз
- **Только реальные проблемы** — сигнал выше шума, не стилистические замечания
- **Контекстный анализ** — анализируем путь выполнения, а не изолированные файлы
- **Специализированные чекеры** — каждый чекер фокусируется на своей области
- **Graceful degradation** — если pointer analysis упал, fallback на CHA; если чекер упал, остальные продолжают работу

---

## 2. Верхнеуровневая архитектура

```
┌─────────────────────────────────────────────────────────────────┐
│                    GitLab (все репозитории)                      │
│                                                                   │
│   MR opened/updated ──────────────────────────────────────────► │
│   Push to main ───────────────────────────────────────────────► │
└──────────────────────────────────┬──────────────────────────────┘
                                   │ System Hook (webhook)
                                   ▼
                    ┌──────────────────────────┐
                    │      Gateway Service      │
                    │  - Верификация подписи    │
                    │  - Определение языка      │
                    │  - Постановка в очередь   │
                    └─────────────┬────────────┘
                                  │
                                  ▼
                    ┌──────────────────────────┐
                    │       Job Queue           │
                    │       (Redis Streams)     │
                    └─────────────┬────────────┘
                                  │
              ┌───────────────────┼───────────────────┐
              ▼                   ▼                   ▼
         ┌─────────┐        ┌─────────┐        ┌─────────┐
         │ Worker  │        │ Worker  │        │ Worker  │
         └────┬────┘        └────┬────┘        └────┬────┘
              │                  │                  │
              └──────────────────┼──────────────────┘
                                 │
                    ┌────────────▼────────────┐
                    │                         │
              ┌─────▼──────┐         ┌───────▼──────┐
              │ Call Graph  │         │   Checker    │
              │   Builder   │────────►│   Pipeline   │
              │  (Go/SSA)   │         │  (7 checkers)│
              └─────────────┘         └───────┬──────┘
                                              │
                                    ┌─────────▼────────┐
                                    │   Claude API      │
                                    │  (batched calls)  │
                                    └─────────┬─────────┘
                                              │
                                    ┌─────────▼────────┐
                                    │  GitLab Reporter  │
                                    │  - Inline comments│
                                    │  - MR summary     │
                                    │  - Labels/blocks  │
                                    └──────────────────┘
```

### Поток данных

```
1. GitLab System Hook → Gateway → очередь (ReviewJob)
2. Worker забирает ReviewJob
3. Git shallow clone изменённой ветки
4. Framework Detector определяет стратегию (Gin / Echo / Chi / net/http)
5. Call Graph Builder строит графы для всех entry points
6. Shared Resource Analyzer находит пересечения (конкурентные проблемы)
7. Checker Pipeline запускает 7 специализированных чекеров параллельно
8. Каждый чекер формирует запрос в Claude API с заточенным промптом
9. Findings агрегируются, дедуплицируются, сортируются по severity
10. GitLab Reporter постит inline comments и summary note
```

---

## 3. Компоненты системы

### 3.1 Gateway Service

**Ответственность:** принимать webhook'и от GitLab, валидировать, ставить задачи в очередь.

**Эндпоинты:**
```
POST /webhook/gitlab        — основной webhook
GET  /health                — healthcheck
GET  /metrics               — Prometheus метрики
```

**Логика обработки webhook:**

```
Входящий запрос
    │
    ├── Проверка X-Gitlab-Token (HMAC)
    ├── Парсинг payload
    ├── Фильтрация:
    │     - object_kind == "merge_request"
    │     - action in ["open", "update", "reopen"]
    │     - НЕ draft MR (опционально, конфигурируемо)
    │     - Язык репо == Go (проверяем через GitLab Languages API или по наличию go.mod)
    └── Enqueue ReviewJob → Redis Streams
```

**ReviewJob структура:**
```go
type ReviewJob struct {
    ID            string
    ProjectID     int
    ProjectURL    string
    MRURL         string
    MRIid         int
    SourceBranch  string
    TargetBranch  string
    DiffRefs      DiffRefs      // base_sha, start_sha, head_sha для inline comments
    ChangedFiles  []string      // список изменённых файлов из webhook payload
    CreatedAt     time.Time
    Priority      int           // payments/* → higher priority
}
```

---

### 3.2 Job Queue

**Технология:** Redis Streams (надёжнее чем Pub/Sub, есть ACK механизм)

**Особенности:**
- Consumer group для распределения между worker'ами
- Dead Letter Queue для задач которые упали 3+ раз
- TTL на задачи: 24 часа (если MR закрыт — анализ не нужен)
- Приоритизация через отдельные стримы: `review:high`, `review:normal`

---

### 3.3 Worker Pool

**Ответственность:** оркестрация полного цикла анализа для одного MR.

```go
type Worker struct {
    queue         QueueClient
    gitlab        GitLabClient
    graphBuilder  *CallGraphBuilder
    checkers      []Checker
    claudeClient  *ClaudeClient
    reporter      *GitLabReporter
    config        *ConfigService
}

func (w *Worker) Process(job ReviewJob) error {
    // 1. Shallow clone только нужной ветки
    repo, err := w.cloneRepo(job)

    // 2. Определяем затронутые пакеты
    affectedPkgs := w.resolveAffectedPackages(job.ChangedFiles, repo)

    // 3. Строим call graphs
    graphs, err := w.graphBuilder.Build(repo, affectedPkgs)

    // 4. Находим конкурентные конфликты
    conflicts := w.analyzeConflicts(graphs)

    // 5. Запускаем чекеры параллельно
    findings := w.runCheckers(graphs, conflicts, repo)

    // 6. Постим результаты
    return w.reporter.Post(job, findings)
}
```

**Масштабирование:** горизонтальное, количество worker'ов конфигурируется. Рекомендуемый старт: 3-5 worker'ов.

---

### 3.4 Call Graph Builder

Детальная архитектура в [разделе 4](#4-call-graph-builder--детальная-архитектура).

**Краткая ответственность:**
- Загрузка Go пакетов через `golang.org/x/tools/go/packages`
- Построение SSA через `golang.org/x/tools/go/ssa`
- Pointer analysis для точного разрешения интерфейсов
- Детектирование entry points (HTTP handlers, Kafka consumers, cron jobs, gRPC handlers)
- Рекурсивный обход call graph с извлечением метаданных (DB calls, TX ops, HTTP calls)

---

### 3.5 Checker Pipeline

Детальная архитектура в [разделе 5](#5-checker-pipeline--детальная-архитектура).

**7 специализированных чекеров:**

| Чекер | Находит |
|-------|---------|
| `TransactionChecker` | Отсутствие транзакций, слишком широкие транзакции |
| `ResilienceChecker` | Таймауты, retry, circuit breaker, outbox pattern |
| `StateMachineChecker` | Невалидные переходы состояний |
| `DataFlowChecker` | Unsanitized input, money fields, IDOR |
| `TemporalOrderChecker` | TOCTOU, неверный порядок операций, partial failure |
| `SchemaConsistencyChecker` | Nullable/индексы/soft delete несоответствия |
| `LoadPatternChecker` | N+1, fan-out, unbounded queries, cache stampede |

Конкурентные конфликты (HTTP vs Kafka) обрабатываются отдельным `ConcurrencyAnalyzer` до запуска чекеров.

---

### 3.6 Claude API Client

```go
type ClaudeClient struct {
    apiKey      string
    model       string    // claude-opus-4-5 для анализа, claude-haiku-4-5 для классификации
    maxTokens   int
    rateLimiter *rate.Limiter
}

// Батчинг: несколько графов в один запрос если влезают в 80k токенов
func (c *ClaudeClient) Analyze(ctx context.Context, req AnalysisRequest) ([]Finding, error)
```

**Модели по задачам:**
- `claude-opus-4-5` — основной анализ бизнес-логики (дорогой, но точный)
- `claude-haiku-4-5` — классификация severity, дедупликация (быстрый и дешёвый)

**Структура ответа от Claude (всегда JSON):**
```json
{
  "findings": [
    {
      "file": "internal/order/service.go",
      "line": 47,
      "severity": "critical",
      "category": "transaction",
      "title": "Multiple DB writes without transaction",
      "description": "OrderRepository.Save() and InventoryRepository.Reserve() called sequentially without transaction. If Reserve() fails, order is already saved — inconsistent state.",
      "suggestion": "Wrap both calls in sql.Tx or use Unit of Work pattern"
    }
  ]
}
```

---

### 3.7 GitLab Reporter

```go
type GitLabReporter struct {
    client *gitlab.Client
    config *ConfigService
}

func (r *GitLabReporter) Post(job ReviewJob, findings []Finding) error {
    // 1. Фильтруем по severity threshold из конфига проекта
    filtered := r.filterBySeverity(findings, job.ProjectID)

    // 2. Inline comments на конкретные строки
    for _, f := range filtered {
        if f.Line > 0 {
            r.createMRDiscussion(job, f)
        }
    }

    // 3. Summary note с общей картиной
    r.createSummaryNote(job, findings)

    // 4. Labels и блокировка MR если нужно
    if r.hasCritical(findings) {
        r.setLabel(job, "ai-review: needs-fix")
        if r.config.BlockMROnCritical(job.ProjectID) {
            r.setMRApprovalRules(job, blocked=true)
        }
    } else {
        r.setLabel(job, "ai-review: passed")
    }
}
```

**Формат inline comment:**
```markdown
🔴 **[CRITICAL] Multiple DB writes without transaction**

`OrderService.CreateOrder()` делает два последовательных обращения к БД без транзакции:
- `OrderRepository.Save()` → line 47
- `InventoryRepository.Reserve()` → line 52 (via InventoryService)

Если `Reserve()` упадёт — заказ уже сохранён, но товар не зарезервирован.

**Исправление:** обернуть оба вызова в `sql.Tx`, передавать через context.

<details><summary>Call path</summary>

`POST /orders` → `OrderService.CreateOrder()` → `InventoryService.Reserve()` → `InventoryRepository.Reserve()`

</details>
```

---

### 3.8 Config Service

Позволяет настраивать поведение на уровне GitLab Group и отдельного проекта.

```yaml
# config/rules.yaml

defaults:
  severity_threshold: high      # Постить в MR findings с severity >= high
  block_mr_on: critical         # Блокировать MR если есть critical findings
  max_findings_per_mr: 25       # Лимит комментариев чтобы не спамить
  checkers:                     # Все чекеры включены по умолчанию
    transaction: true
    resilience: true
    state_machine: true
    data_flow: true
    temporal: true
    schema: true
    load_pattern: true

overrides:
  # Legacy репо — снижаем строгость
  - match: "group/legacy-*"
    severity_threshold: critical
    block_mr_on: never
    checkers:
      schema: false             # Миграции там не по стандарту

  # Payment сервисы — максимальная строгость
  - match: "group/payments/*"
    severity_threshold: medium
    block_mr_on: high
    extra_checks:
      - double_spend
      - idempotency_key
      - money_flow

  # Новые сервисы — стандарт
  - match: "group/platform/*"
    severity_threshold: high
    block_mr_on: critical
```

---

## 4. Call Graph Builder — детальная архитектура

### Стек технологий

```
golang.org/x/tools/go/packages    — загрузка пакетов с типами
golang.org/x/tools/go/ssa         — SSA представление (Static Single Assignment)
golang.org/x/tools/go/ssa/ssautil — утилиты для SSA
golang.org/x/tools/go/callgraph/cha — CHA (Class Hierarchy Analysis, быстрый, seed для VTA)
golang.org/x/tools/go/callgraph/vta — VTA (Variable Type Analysis, рафинирует CHA)
// NOTE: PTA (Pointer Analysis) заменён на CHA→VTA в реализации — стабильнее, без timeout'ов
```

### Алгоритм построения

```
┌─────────────────────────────────────────────────────────┐
│                    Call Graph Builder                    │
│                                                          │
│  1. packages.Load(./...)                                 │
│        │                                                 │
│        ▼                                                 │
│  2. ssautil.AllPackages() → prog.Build()                 │
│        │                                                 │
│        ▼                                                 │
│  3. cha.CallGraph() → vta.CallGraph() — точный call graph │
│     (реализовано через CHA→VTA; PTA заменён на VTA)      │
│        │                                                 │
│        ▼                                                 │
│  4. EntryPointFinder.FindAll()                           │
│     - HTTP handlers (Gin/Echo/Chi/net/http)              │
│     - Kafka consumers (sarama/confluent/segmentio)       │
│     - gRPC handlers                                      │
│     - Cron jobs (robfig/cron, go-co-op/gocron)          │
│        │                                                 │
│        ▼                                                 │
│  5. Для каждого entry point:                             │
│     GraphBuilder.buildNode(fn, depth=0, maxDepth=6)      │
│     - Обходим рёбра call graph (из pta результата)       │
│     - Фильтруем stdlib и runtime шум                     │
│     - Извлекаем метаданные каждого узла                  │
│        │                                                 │
│        ▼                                                 │
│  6. Serialize() → JSON для Claude                        │
│     - Дерево вызовов с исходным кодом каждого узла      │
│     - Метаданные: DB calls, TX ops, HTTP calls           │
└─────────────────────────────────────────────────────────┘
```

### Детектирование entry points

#### HTTP Handlers

```go
// Паттерны для детектирования регистрации роутов
var httpRoutePatterns = map[string]FrameworkMeta{
    // Gin
    "(*github.com/gin-gonic/gin.RouterGroup).GET":    {Framework: "gin", Method: "GET"},
    "(*github.com/gin-gonic/gin.RouterGroup).POST":   {Framework: "gin", Method: "POST"},
    "(*github.com/gin-gonic/gin.RouterGroup).PUT":    {Framework: "gin", Method: "PUT"},
    "(*github.com/gin-gonic/gin.RouterGroup).DELETE": {Framework: "gin", Method: "DELETE"},
    "(*github.com/gin-gonic/gin.RouterGroup).PATCH":  {Framework: "gin", Method: "PATCH"},

    // Echo
    "(*github.com/labstack/echo/v4.Echo).GET":  {Framework: "echo", Method: "GET"},
    "(*github.com/labstack/echo/v4.Echo).POST": {Framework: "echo", Method: "POST"},
    "(*github.com/labstack/echo/v4.Group).GET": {Framework: "echo", Method: "GET"},

    // Chi
    "(*github.com/go-chi/chi/v5.Mux).Get":    {Framework: "chi", Method: "GET"},
    "(*github.com/go-chi/chi/v5.Mux).Post":   {Framework: "chi", Method: "POST"},

    // net/http stdlib
    "net/http.HandleFunc":                        {Framework: "stdlib", Method: "ANY"},
    "(*net/http.ServeMux).HandleFunc":            {Framework: "stdlib", Method: "ANY"},
}
```

#### Kafka Consumers

```go
var kafkaConsumerPatterns = []string{
    // sarama (Shopify)
    "ConsumeClaim",   // метод интерфейса ConsumerGroupHandler
    "(*github.com/Shopify/sarama.ConsumerGroup).Consume",

    // confluent-kafka-go
    "(*github.com/confluentinc/confluent-kafka-go/kafka.Consumer).Poll",

    // segmentio/kafka-go
    "(*github.com/segmentio/kafka-go.Reader).ReadMessage",
    "(*github.com/segmentio/kafka-go.Reader).FetchMessage",

    // IBM/sarama (новое имя пакета)
    "(*github.com/IBM/sarama.ConsumerGroup).Consume",
}

// Дополнительно извлекаем topic из аргументов
type KafkaEntryPoint struct {
    EntryPoint
    Topic         string
    ConsumerGroup string
    CommitMode    string   // auto / manual
}
```

#### gRPC Handlers

```go
// Ищем регистрацию сервисов через RegisterXxxServer паттерн
// и маппим на методы proto-сервиса через reflection
var grpcPatterns = []string{
    "google.golang.org/grpc.(*Server).RegisterService",
}
```

### Метаданные узлов

Для каждого узла графа извлекаем:

```go
type NodeMetadata struct {
    // DB операции
    DBCalls []DBCall{
        Package string    // "database/sql", "gorm.io/gorm", "sqlx"
        Method  string    // Query, Exec, Get, Find...
        Query   string    // SQL если это строковый литерал
        Line    int
    }

    // Транзакционные операции
    TxOps []TxOp{
        Operation string  // Begin, BeginTx, Commit, Rollback, Transaction (gorm)
        Line      int
    }

    // Исходящие HTTP вызовы
    HTTPCalls []HTTPCall{
        Line       int
        HasTimeout bool   // проверяем через &http.Client{Timeout: ...}
        IsRetried  bool   // есть ли retry wrapper
    }

    // Kafka publish
    KafkaPublish []KafkaPublish{
        Topic string
        Line  int
        InsideTx bool  // publish внутри транзакции (проблема!)
    }

    // Mutex / sync операции
    SyncOps []SyncOp{
        Type string   // Mutex.Lock, sync.Once, singleflight
        Line int
    }

    // Redis операции (для distributed lock детектирования)
    RedisOps []RedisOp{
        Method string   // SetNX, Eval, ...
        Line   int
    }
}
```

### Управление контекстом при обходе

Pointer analysis на больших репо может занимать много времени. Оптимизации:

**Scope limiting** — загружаем только пакеты затронутые диффом MR + их транзитивные зависимости до глубины 3:

```go
func resolveScope(changedFiles []string, repoRoot string) []string {
    // Определяем пакеты изменённых файлов
    changedPkgs := filesToPackages(changedFiles)

    // Транзитивные зависимости до глубины 3
    allPkgs := expandDeps(changedPkgs, depth=3)

    return allPkgs
}
```

**Timeout на pointer analysis** — если pta не завершился за 3 минуты, fallback на CHA:

```go
ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
result, err := pta.Analyze(config)
if err != nil || ctx.Err() != nil {
    log.Warn("pta timeout, falling back to CHA")
    result = cha.CallGraph(prog)
}
```

**Visited set** — предотвращаем зацикливание при рекурсивных вызовах:

```go
// Sliding window: посещённые функции сбрасываются между разными ветками дерева
// Но не между вызовами одного и того же узла из разных путей
func (b *GraphBuilder) buildNode(fn *ssa.Function, depth int, visited visitedSet) *CallNode {
    if visited.has(fn) || depth > maxDepth {
        return &CallNode{Truncated: true}
    }
    visited.add(fn)
    defer visited.remove(fn)
    // ...
}
```

---

## 5. Checker Pipeline — детальная архитектура

### Интерфейс чекера

```go
type Checker interface {
    Name() string
    Check(ctx context.Context, input CheckerInput) ([]Finding, error)
}

type CheckerInput struct {
    // Call graphs для всех entry points
    Graphs []CallGraph

    // Конкурентные конфликты (результат SharedResourceAnalyzer)
    Conflicts []ConcurrencyConflict

    // Исходный код файлов (для контекста Claude)
    SourceFiles map[string]string

    // DB схема если доступна
    Schema *DBSchema

    // Конфиг проекта
    Config ProjectConfig
}
```

### Параллельный запуск

```go
func (p *Pipeline) Run(ctx context.Context, input CheckerInput) []Finding {
    results := make(chan []Finding, len(p.checkers))

    for _, checker := range p.checkers {
        go func(c Checker) {
            findings, err := c.Check(ctx, input)
            if err != nil {
                log.Error("checker failed", "name", c.Name(), "err", err)
                findings = nil
            }
            results <- findings
        }(checker)
    }

    var all []Finding
    for range p.checkers {
        all = append(all, <-results...)
    }

    return p.dedup(p.sort(all))
}
```

### Детали каждого чекера

#### TransactionChecker

**Что ищет:** несколько DB-операций в одном пути без транзакции; транзакция открыта слишком широко (внутри неё есть HTTP-вызов).

**Алгоритм:**
1. Для каждого пути от entry point до листьев — собираем все DB calls и TX ops
2. Если DBCalls > 1 и TxOps пустой → finding "Missing transaction"
3. Если TxOps.Begin найден → смотрим что находится между Begin и Commit; если есть HTTPCall → finding "Transaction too broad"
4. Если TxOps.Begin без парного Commit/Rollback в явном виде → проверяем `defer tx.Rollback()` паттерн

**Промпт Claude:**
```
Тебе дан call graph HTTP endpoint с метаданными DB операций и транзакций.

DB calls: {list of db calls with lines}
TX operations: {Begin/Commit/Rollback with lines}
HTTP calls inside the path: {list}

Найди:
1. Есть ли несколько DB операций без объединяющей транзакции?
   Если да — какие именно операции должны быть атомарными и почему?
2. Есть ли HTTP вызов внутри транзакции (между Begin и Commit)?
   Если да — это проблема: транзакция держит соединение пока идёт сетевой вызов.
3. Есть ли Begin без гарантированного Rollback при ошибке?

Отвечай только JSON.
```

---

#### ResilienceChecker

**Что ищет:** HTTP вызовы без таймаута; отсутствие retry на идемпотентных операциях; Kafka publish без outbox pattern; отсутствие circuit breaker.

**Алгоритм:**
1. Для каждого HTTPCall проверяем `HasTimeout` — извлекается на этапе построения графа через анализ `http.Client{Timeout: ...}`
2. Для Kafka publish проверяем `InsideTx` — если publish внутри транзакции, это неправильно (при откате транзакции сообщение уже ушло)
3. Ищем паттерны retry wrapper библиотек: `github.com/avast/retry-go`, `github.com/cenkalti/backoff`

---

#### StateMachineChecker

**Что ищет:** невалидные переходы состояний, переходы из терминальных состояний, отсутствие проверки текущего статуса перед переходом.

**Алгоритм:**
1. Находим поля типа `status`, `state` в DB схеме (или выводим из кода)
2. Из всех путей выполнения извлекаем UPDATE запросы меняющие статус
3. Строим граф переходов: `pending → confirmed`, `pending → cancelled`, `confirmed → shipped`
4. Ищем: переход из терминального состояния; переход без проверки текущего состояния (UPDATE без WHERE status=?)
5. Передаём граф переходов в Claude для анализа бизнес-корректности

**Пример finding:**
```
🔴 CRITICAL: Transition from terminal state
cancelled → shipped transition is possible via POST /shipments
No check for current status before transition at OrderRepository.UpdateStatus() line 89
```

---

#### DataFlowChecker

**Что ищет:** unsanitized input в DB запросах; поля с деньгами без валидации; IDOR (отсутствие фильтрации по owner ID).

**Алгоритм:**
1. Из entry point извлекаем источники входных данных (HTTP body, query params, path params)
2. Трассируем переменные через SSA: где они используются в DB calls
3. Проверяем наличие валидации между источником и использованием
4. Для IDOR: ищем userID из JWT/session и проверяем есть ли он в WHERE clause всех SELECT/UPDATE/DELETE

---

#### TemporalOrderChecker

**Что ищет:** TOCTOU паттерны; неверный порядок операций; что происходит при partial failure.

**Алгоритм:**
1. Строим timeline операций в рамках одного пути: SELECT → validate → UPDATE
2. Ищем паттерн: SELECT (read) → бизнес-логика → UPDATE (write) без FOR UPDATE или WHERE version=N
3. Строим "failure map": если шаг N упал — что уже выполнено, есть ли компенсация
4. Передаём в Claude с вопросом: "что произойдёт если операция X упадёт после Y но до Z?"

---

#### SchemaConsistencyChecker

**Что ищет:** несоответствия между Go структурами и DB схемой.

**Входные данные:** файлы миграций (`.sql` файлы или `golang-migrate` структура) + Go structs.

**Алгоритм:**
1. Парсим миграции: извлекаем nullable/not-null для каждой колонки, индексы, constraints
2. Маппим на Go structs через теги (`db:"column_name"`, `gorm:"column:column_name"`)
3. Проверяем: nullable колонка → должен быть `*Type` или `sql.NullXxx` в Go
4. Из DB calls извлекаем паттерны запросов (WHERE clause) и сверяем с индексами
5. Проверяем наличие `deleted_at IS NULL` в запросах к таблицам с soft delete

---

#### LoadPatternChecker

**Что ищет:** N+1 запросы, unbounded queries, fan-out, cache stampede.

**Алгоритм:**
1. Ищем DB call внутри цикла (for loop в SSA содержит basic block с DB call)
2. Ищем SELECT без LIMIT
3. Ищем паттерн: HTTP запрос принимает массив ID → делает N DB запросов
4. Ищем cache read → miss → DB → cache write без singleflight защиты

---

## 6. Анализ конкурентных точек входа

`SharedResourceAnalyzer` запускается после построения всех графов, до запуска чекеров.

### Алгоритм

```
Все entry points и их call graphs
        │
        ▼
Инвертированный индекс:
  "OrderService.UpdateStatus" → [HTTPEntryPoint, KafkaEntryPoint]
  "orders table"              → [HTTPEntryPoint, KafkaEntryPoint, CronEntryPoint]
        │
        ▼
Фильтр: ресурс → 2+ entry points, хотя бы один WRITE
        │
        ▼
Для каждого конфликта:
  - Классифицируем источники: HTTP / Kafka / Cron / gRPC
  - Проверяем наличие защиты (FOR UPDATE, optimistic lock, distributed lock, atomic UPDATE)
  - Для Kafka: проверяем commit mode, idempotency, at-least-once implications
        │
        ▼
ConcurrencyConflict объекты → в CheckerInput для всех чекеров
```

### Детектирование защитных механизмов

```go
type ConcurrencyProtection struct {
    HasOptimisticLock   bool   // WHERE version=N или WHERE updated_at=T
    HasPessimisticLock  bool   // SELECT FOR UPDATE
    HasAtomicUpdate     bool   // UPDATE ... WHERE status='pending' (без SELECT)
    HasDistributedLock  bool   // Redis SetNX, Redlock
    HasUniqueConstraint bool   // INSERT ON CONFLICT / уникальный индекс
    HasIdempotencyKey   bool   // уникальный ключ на операцию
}
```

### Специальный промпт для конкурентных конфликтов

```
Два entry point обращаются к одному ресурсу:
  - HTTP: POST /orders/confirm (call tree: ...)
  - Kafka: topic=order.payment.confirmed (call tree: ...)

Оба вызывают OrderService.UpdateStatus() и пишут в таблицу orders.

Kafka использует at-least-once доставку — сообщение может прийти дважды.
Commit mode: auto — при падении после обработки до commit сообщение придёт снова.

Защитные механизмы: {found protections or "none"}

Найди:
1. Возможен ли lost update (оба прочли старый статус, оба записали свой)?
2. Возможны ли невалидные state transitions при параллельном выполнении?
3. Что произойдёт если Kafka сообщение обработается дважды?
4. Достаточна ли текущая защита? Если нет — что именно добавить?
```

---

## 7. Схема данных

### Finding

```go
type Finding struct {
    ID          string
    CheckerName string
    
    // Локация
    File        string
    Line        int
    Function    string
    
    // Контент
    Severity    Severity    // critical, high, medium, low
    Category    Category    // transaction, resilience, state_machine, data_flow, temporal, concurrency, schema, load
    Title       string
    Description string
    Suggestion  string
    
    // Для inline комментария в GitLab
    CallPath    []string    // путь от entry point до проблемного места
    
    // Дедупликация
    Hash        string      // hash(file+line+category) для дедупликации между чекерами
}
```

### CallGraph

```go
type CallGraph struct {
    EntryPoint EntryPoint
    Root       *CallNode
}

type EntryPoint struct {
    Source    EntryPointSource   // http, kafka, grpc, cron
    Method    string             // GET, POST, ...
    Path      string             // /api/orders или topic name
    Function  *ssa.Function
    File      string
    Line      int
    
    // Kafka-специфика
    Topic         string
    ConsumerGroup string
    CommitMode    string
}

type CallNode struct {
    Function    *ssa.Function
    File        string
    Line        int
    Category    NodeCategory    // handler, service, repository, http_client, producer
    SourceCode  string
    
    DBCalls     []DBCall
    TxOps       []TxOp
    HTTPCalls   []HTTPCall
    KafkaOps    []KafkaOp
    SyncOps     []SyncOp
    
    Children    []*CallEdge
    Truncated   bool    // если достигли maxDepth
}
```

---

## 8. Структура репозитория

```
ai-code-reviewer/
│
├── cmd/
│   ├── gateway/          # main.go для Gateway Service
│   └── worker/           # main.go для Worker
│
├── internal/
│   ├── gateway/
│   │   ├── handler.go    # HTTP handler для GitLab webhooks
│   │   └── validator.go  # HMAC валидация
│   │
│   ├── queue/
│   │   ├── producer.go   # Постановка задач в Redis Streams
│   │   └── consumer.go   # Чтение задач воркером
│   │
│   ├── worker/
│   │   └── worker.go     # Основной оркестратор
│   │
│   ├── graph/            # Call Graph Builder
│   │   ├── builder.go    # Основной builder (SSA + pta)
│   │   ├── entrypoints/
│   │   │   ├── http.go   # Детектор HTTP handlers
│   │   │   ├── kafka.go  # Детектор Kafka consumers
│   │   │   ├── grpc.go   # Детектор gRPC handlers
│   │   │   └── cron.go   # Детектор cron jobs
│   │   ├── metadata.go   # Извлечение DB/TX/HTTP метаданных из SSA
│   │   ├── serializer.go # Сериализация в JSON для Claude
│   │   └── concurrency.go # SharedResourceAnalyzer
│   │
│   ├── checkers/
│   │   ├── interface.go
│   │   ├── pipeline.go
│   │   ├── transaction.go
│   │   ├── resilience.go
│   │   ├── statemachine.go
│   │   ├── dataflow.go
│   │   ├── temporal.go
│   │   ├── schema.go
│   │   └── loadpattern.go
│   │
│   ├── claude/
│   │   ├── client.go     # Claude API client с батчингом
│   │   └── prompts/      # Промпты для каждого чекера
│   │       ├── transaction.md
│   │       ├── resilience.md
│   │       ├── statemachine.md
│   │       ├── dataflow.md
│   │       ├── temporal.md
│   │       ├── concurrency.md
│   │       ├── schema.md
│   │       └── loadpattern.md
│   │
│   ├── gitlab/
│   │   ├── client.go     # GitLab API client
│   │   └── reporter.go   # Форматирование и постинг findings
│   │
│   └── config/
│       ├── loader.go     # Загрузка rules.yaml
│       └── resolver.go   # Матчинг проекта с конфигом
│
├── config/
│   └── rules.yaml        # Конфигурация по умолчанию + overrides
│
├── helm/                 # Helm chart для деплоя в k8s
│   ├── Chart.yaml
│   ├── values.yaml
│   └── templates/
│
├── docker/
│   ├── Dockerfile.gateway
│   └── Dockerfile.worker
│
├── go.mod
├── go.sum
└── README.md
```

---

## 9. Деплой и инфраструктура

### Kubernetes

```yaml
# Gateway: легковесный, статeless
gateway:
  replicas: 2
  resources:
    requests: { cpu: "100m", memory: "128Mi" }
    limits:   { cpu: "500m", memory: "256Mi" }

# Workers: тяжёлые (SSA + pointer analysis жрёт RAM)
worker:
  replicas: 3
  resources:
    requests: { cpu: "1000m", memory: "2Gi" }
    limits:   { cpu: "4000m", memory: "8Gi" }
```

### Переменные окружения

```bash
# GitLab
GITLAB_URL=https://gitlab.company.com
GITLAB_TOKEN=<service-account-token>   # read_api + write_repository
GITLAB_WEBHOOK_SECRET=<hmac-secret>

# Claude API
ANTHROPIC_API_KEY=<key>
CLAUDE_MODEL_ANALYSIS=claude-opus-4-5
CLAUDE_MODEL_CLASSIFY=claude-haiku-4-5

# Redis
REDIS_URL=redis://redis:6379

# Config
CONFIG_PATH=/etc/reviewer/rules.yaml
```

### GitLab настройка (один раз)

```
GitLab Admin → System Hooks → Add hook:
  URL: https://ai-reviewer.internal/webhook/gitlab
  Secret Token: <GITLAB_WEBHOOK_SECRET>
  Triggers: ✅ Merge request events
```

Один System Hook покрывает все репозитории в GitLab инстансе.

---

## 10. Конфигурация

### Полный rules.yaml

```yaml
defaults:
  severity_threshold: high
  block_mr_on: critical
  max_findings_per_mr: 25
  skip_draft_mrs: true
  
  checkers:
    transaction:    { enabled: true }
    resilience:     { enabled: true }
    state_machine:  { enabled: true }
    data_flow:      { enabled: true }
    temporal:       { enabled: true }
    schema:         { enabled: true, migrations_path: "migrations/" }
    load_pattern:   { enabled: true }
  
  graph:
    max_depth: 6
    pta_timeout_seconds: 180
    scope_expansion_depth: 3    # транзитивные зависимости для scope limiting

overrides:
  - match: "group/payments/*"
    severity_threshold: medium
    block_mr_on: high
    checkers:
      data_flow:
        extra_checks: [money_flow, double_spend]
      transaction:
        extra_checks: [idempotency_key]

  - match: "group/legacy-*"
    severity_threshold: critical
    block_mr_on: never
    checkers:
      schema: { enabled: false }
    graph:
      max_depth: 4     # legacy код глубже не идём — слишком шумно
```

---

## 11. Производительность и оптимизации

### Время анализа (ориентиры)

| Этап | Время |
|------|-------|
| Git shallow clone | 5–30 сек |
| packages.Load + SSA build | 30–90 сек |
| Pointer analysis | 30–180 сек |
| Call graph traversal | 5–30 сек |
| Claude API (все чекеры параллельно) | 20–60 сек |
| GitLab posting | 5–15 сек |
| **Итого** | **~2–6 мин** |

### Оптимизации

**Кэширование SSA** — если два MR в одном репо анализируются параллельно, SSA строится один раз и переиспользуется (с TTL 1 час):

```
Cache key: repo_url + commit_sha → SSA Program
```

**Инкрементальный анализ** — для `update` MR события анализируем только изменившиеся файлы относительно предыдущего коммита, не весь MR.

**Батчинг Claude запросов** — несколько малых call graphs объединяются в один запрос до лимита ~80k токенов. Это снижает latency и стоимость.

**Параллельный запуск чекеров** — все 7 чекеров работают параллельно, общее время = время самого медленного.

---

## 12. Мониторинг

### Метрики (Prometheus)

```
# Бизнес-метрики
reviewer_jobs_total{status="success|failed|timeout"}
reviewer_findings_total{severity="critical|high|medium", category="..."}
reviewer_analysis_duration_seconds{phase="clone|ssa|pta|checkers|claude|post"}

# Производительность
reviewer_claude_tokens_used_total{checker="..."}
reviewer_claude_cost_usd_total
reviewer_queue_depth
reviewer_worker_busy_count
```

### Алерты

```yaml
- alert: ReviewerHighErrorRate
  expr: rate(reviewer_jobs_total{status="failed"}[5m]) > 0.1
  
- alert: ReviewerQueueBacklog
  expr: reviewer_queue_depth > 50
  
- alert: ReviewerAnalysisSlow
  expr: histogram_quantile(0.95, reviewer_analysis_duration_seconds) > 600
```

---

## 13. Roadmap

### v1.0 — MVP

- Gateway Service + Redis Queue + Worker
- Call Graph Builder (HTTP + Kafka, Gin/Echo/Chi)
- TransactionChecker + ResilienceChecker
- GitLab inline comments + summary

### v1.1 — Расширение чекеров

- StateMachineChecker
- DataFlowChecker (IDOR, money flow)
- ConcurrencyAnalyzer (HTTP vs Kafka конфликты)

### v1.2 — Schema & Load

- SchemaConsistencyChecker (nullable, soft delete, индексы)
- LoadPatternChecker (N+1, unbounded queries)
- TemporalOrderChecker

### v2.0 — Cross-service анализ

- Анализ нескольких репозиториев вместе
- Event schema drift детектирование (Kafka topics)
- API contract validation между сервисами
- Исторический трекинг: "эта проблема была найдена 3 раза — паттерн"

### v2.1 — Обучение на кодовой базе

- Fine-tuning промптов на реальных findings (что было принято, что проигнорировано)
- Автоматическое снижение severity для false-positive паттернов
- Suggested fixes в виде diff патчей

---

*Документ актуален на февраль 2026. Архитектура рассчитана на Go монорепо и multi-repo в GitLab с единым System Hook.*