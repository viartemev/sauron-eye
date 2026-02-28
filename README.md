# Sauron Eye — Go Call Graph Analyser & AI Code Reviewer

Static call-graph builder for Go projects with an interactive web visualiser
and an AI-powered code review tool.

Builds a full SSA call graph (CHA → VTA), detects entry points (HTTP handlers,
Kafka consumers, gRPC handlers, cron jobs), extracts metadata (DB calls,
transactions, HTTP calls, Kafka ops, Redis ops, channel sends/receives, deferred
calls), and serialises everything to JSON.

The JSON can be explored in the web UI or sent to Claude for a structured review
against a configurable checklist of production reliability and security rules.

---

## Tools

| Tool | What it does |
|------|-------------|
| `callgraph` | Build a call graph and write it as JSON (used by the web UI) |
| `review` | Interactive: pick an entry point and get a Claude AI review |

---

## Requirements

| Requirement | Details |
|------------|---------|
| Go | 1.26+ |
| Project to analyse | Any module-based Go project |
| `ANTHROPIC_API_KEY` | Only required for `review` — set in environment or via `-api-key` flag |

No external services needed for `callgraph`. The web UI is a single static HTML file.

---

## Quick start — call graph + web UI

```bash
# 1. Clone and build
git clone <repo-url> sauron-eye
cd sauron-eye
go build -o callgraph ./cmd/callgraph

# 2. Analyse a project
./callgraph -output graph.json /path/to/your/go/project

# 3. Open the visualiser, drop graph.json onto the page
open web/index.html          # macOS
xdg-open web/index.html      # Linux
```

## Quick start — AI review

```bash
go build -o review ./cmd/review

export ANTHROPIC_API_KEY=sk-ant-...

# Run against a project — interactive entry point selection
./review /path/to/your/go/project

# Non-interactive (pre-select entry point 3, save output)
./review -select 3 -output review.md /path/to/your/go/project
```

---

## `callgraph` — CLI reference

```
callgraph [flags] <project-path>
```

`<project-path>` is the root directory of the Go project to analyse (must
contain `go.mod`). Defaults to `.` if omitted.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-max-depth` | `15` | Maximum call-tree traversal depth. Deeper = more complete graph but slower and larger output. |
| `-scope` | `./...` | Comma-separated package patterns passed to `go/packages`. Restrict to a sub-tree to speed up analysis (e.g. `./internal/...`). |
| `-pkg-filter` | module name | Comma-separated package path prefixes to recurse into. Nodes outside the filter are recorded but not expanded. |
| `-output` | stdout | File path to write JSON to. If omitted, JSON is written to stdout. |
| `-format` | `pretty` | `pretty` — indented JSON; `json` — compact JSON. |
| `-no-source` | false | Strip source-code snippets from output. Reduces file size significantly; detail panel in the web UI will be empty. |
| `-skip-generated` | false | Do not recurse into auto-generated files (`*.pb.go`, `*_gen.go`, files with `// Code generated` header). |
| `-verbose` | false | Print progress lines to stderr (package count, SSA build, entry-point count, timings). |

### Examples

```bash
# Minimal — pretty JSON to stdout
./callgraph ./my-service

# Write compact JSON to a file, verbose progress
./callgraph -format json -verbose -output graph.json ./my-service

# Analyse only a sub-package, increase depth
./callgraph -scope ./internal/order/... -max-depth 20 -output order.json ./my-service

# Skip source code to keep the file small
./callgraph -no-source -output graph.json ./my-service

# Pipe into jq
./callgraph -format json ./my-service | jq '.graphs | length'
```

---

## `review` — CLI reference

```
review [flags] [project-path]
```

Builds the call graph, presents a numbered list of all detected entry points,
lets you pick one, then sends its full call tree (plus relevant concurrency
conflicts and the project's `checks.md`) to Claude for a structured review.

`project-path` defaults to `.`.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-api-key` | `$ANTHROPIC_API_KEY` | Anthropic API key. |
| `-model` | `claude-opus-4-6` | Claude model to use. |
| `-max-depth` | `15` | Maximum call-tree traversal depth. |
| `-checks` | `<project>/checks.md` | Path to the checklist file sent to the LLM. |
| `-select` | 0 (interactive) | Pre-select entry point by number, skipping the interactive prompt. Useful in CI. |
| `-no-source` | false | Strip source code from the call graph before sending to Claude. Reduces token usage. |
| `-output` | stdout | Write the LLM response to a file instead of stdout. |
| `-verbose` | false | Print token usage and prompt sizes to stderr. |

### Examples

```bash
# Interactive — shows list, prompts for selection
./review ./my-service

# Non-interactive — entry point 3, save to file
./review -select 3 -output review.md ./my-service

# Cheaper/faster model, skip source code
./review -model claude-haiku-4-5-20251001 -no-source ./my-service

# Custom checks file
./review -checks ./config/my-checks.md ./my-service

# Verbose — shows token counts
./review -verbose -select 1 ./my-service
```

### Example session

```
Building call graph for /path/to/orders-service …
(this typically takes 30–90 seconds)

Module: github.com/company/orders-service
Found 7 entry point(s):

  [ 1]  HTTP POST    /api/orders                      → OrderHandler.CreateOrder
  [ 2]  HTTP GET     /api/orders/:id                  → OrderHandler.GetOrder
  [ 3]  HTTP DELETE  /api/orders/:id                  → OrderHandler.DeleteOrder
  [ 4]  HTTP GET     /api/orders                      → OrderHandler.ListOrders
  [ 5]  Kafka   topic=order.payment.confirmed         → PaymentConsumer.Handle
  [ 6]  Kafka   topic=inventory.reserved              → InventoryConsumer.Handle
  [ 7]  Cron    sync-pending-orders                   → Scheduler.SyncPending

Enter number (1–7): 1

Selected: HTTP POST /api/orders → OrderHandler.CreateOrder
Sending to Claude (claude-opus-4-6) …

### [CRITICAL] Multiple DB writes without transaction

**Category:** Transactions
**Location:** `internal/order/service.go:52` in `(*Service).CreateOrder()`

OrderRepository.Save() and InventoryRepository.Reserve() are called
sequentially without a wrapping transaction. If Reserve() fails, the order
is already committed — inconsistent state.

**Fix:** Wrap both calls in a `sql.Tx` obtained from the DB, pass it through
context or as an explicit argument to both repositories.

**Call path:** POST /api/orders → OrderHandler.CreateOrder → Service.CreateOrder
→ InventoryService.Reserve → InventoryRepository.Reserve
```

---

## `checks.md` — the review checklist

`checks.md` at the **analysed project's** root defines what the LLM looks for in every review.
It is plain Markdown — edit it to add, remove, or tune rules for your codebase.

If no `checks.md` is found the tool warns and continues with a generic Go best-practices prompt.

A typical file covers eight categories:

| Category | Examples |
|----------|---------|
| **Transactions** | Missing TX, TX too broad, no rollback on error |
| **Resilience** | HTTP without timeout, Kafka publish inside TX, no retry |
| **State Machine** | Transition from terminal state, no guard on current status |
| **Data Flow** | IDOR, unsanitized input, money field without validation |
| **Temporal Order** | TOCTOU, wrong operation order, no compensation on partial failure |
| **Concurrency** | HTTP vs Kafka race, double-spend, non-idempotent Kafka consumer |
| **Schema** | Nullable column as non-pointer, soft-delete filter missing |
| **Load Patterns** | N+1 queries, unbounded SELECT, cache stampede |

You can maintain different check files per environment:

```bash
# Strict (payments service)
./review -checks checks-payments.md ./payments-service

# Relaxed (legacy service)
./review -checks checks-legacy.md ./legacy-service
```

---

## Web visualiser

Open `web/index.html` in any modern browser (Chrome, Firefox, Safari).

### Loading data

- **Drag & drop** a `graph.json` file onto the drop zone, or
- **Click** the drop zone to pick a file, or
- **Paste** raw JSON into the text area and click **Analyse →**

### Navigation

| Action | How |
|--------|-----|
| Select entry point | Click any item in the left sidebar |
| Pan | Click and drag on the canvas |
| Zoom | Scroll wheel |
| Select a node | Click the node — opens the detail panel on the right |
| Deselect | Click the canvas background |
| Close detail panel | Click × in the panel header |
| Filter out stdlib entry points | Toggle **hide stdlib** in the sidebar header |
| Load a new file | Click **↩ Load another** in the top bar |

### Node colours

| Colour | Category |
|--------|----------|
| Purple | `handler` — HTTP/gRPC/Kafka entry-point handlers |
| Blue | `service` — business logic layer |
| Green | `repository` — data access layer |
| Amber | `http_client` — outgoing HTTP |
| Red | `producer` — Kafka producers |
| Grey | `unknown` |

### Edge styles

| Style | Meaning |
|-------|---------|
| Solid grey | Regular function call |
| Dashed purple | Channel send → consumer (async handoff) |
| Dashed amber | `defer`-ed call |

### Node badges

| Badge | Meaning |
|-------|---------|
| `⬡ N db` | N database calls inside this function |
| `↗ http` | Outgoing HTTP call |
| `⟳ kafka` | Kafka publish or consume |
| `tx` | Transaction operation (Begin / Commit / Rollback) |
| `→ chan` | Channel send |
| `← chan` | Channel receive |

A **`defer`** tag in the top-right corner of a node means this function is
called via a `defer` statement in its parent.

### Detail panel

Click any node to see:
- Category and file location
- All metadata badges
- DB calls with SQL query (if statically extractable) and line number
- Transaction operations
- Outgoing HTTP calls with timeout status
- Kafka ops with topic and whether the publish is inside a transaction
- Channel ops (send / receive) with type and line
- Source code of the function (unless `-no-source` was used)

### Concurrency conflicts bar

If the analyser detects two or more entry points writing to the same resource
(table, shared state), a bar appears at the bottom of the screen listing the
conflicts and what protections (pessimistic lock, optimistic lock, atomic
update, etc.) were found.

---

## Output format

The JSON output is a `BuildResult`:

```json
{
  "module": "github.com/company/my-service",
  "graphs": [ ... ],         // one CallGraph per entry point
  "used_vta": true,          // false = fell back to CHA only
  "errors": [ ... ],         // non-fatal warnings from the analysis
  "conflicts": [ ... ]       // concurrency conflicts detected
}
```

Each `CallGraph`:

```json
{
  "entry_point": {
    "source": "http",        // http | kafka | grpc | cron
    "method": "POST",
    "path": "/api/orders",
    "function_name": "...",
    "file": "...", "line": 42,
    "topic": "",             // Kafka only
    "consumer_group": "",
    "commit_mode": ""
  },
  "root": { /* CallNode */ }
}
```

Each `CallNode`:

```json
{
  "function_name": "(*pkg.Service).CreateOrder",
  "package": "myapp/internal/order",
  "file": "/abs/path/service.go",
  "line": 47,
  "category": "service",
  "generated": false,     // true when the source file is auto-generated (*.pb.go, *_gen.go, etc.)
  "source_code": "func (s *Service) ...",   // up to 150 lines; omitted with -no-source
  "db_calls":    [ { "package": "...", "method": "Exec", "query": "INSERT ...", "line": 51 } ],
  "tx_ops":      [ { "operation": "Begin", "line": 49 } ],
  "http_calls":  [ { "line": 88, "has_timeout": true, "is_retried": false } ],
  "kafka_ops":   [ { "topic": "orders", "line": 95, "is_publish": true, "inside_tx": false } ],
  "sync_ops":    [ { "type": "Mutex.Lock", "line": 60 } ],
  "redis_ops":   [ { "method": "SetNX", "line": 73 } ],
  "channel_ops": [ { "direction": "send", "chan_type": "myapp/pkg.Job", "line": 102 } ],
  "children": [
    {
      "callee": { /* CallNode */ },
      "line": 51,
      "via": "channel",   // omitted for regular calls; "channel" for async handoff
      "deferred": true    // omitted when false
    }
  ],
  "truncated": false,
  "trunc_reason": ""      // "depth" or "cycle" when truncated=true
}
```

---

## Depth tuning

`-max-depth` controls how many call levels are expanded from each entry point.

| Value | When to use |
|-------|-------------|
| `6–10` | Quick review, large mono-repos, or when you only care about the top layers |
| `15` (default) | Good balance for most services |
| `20+` | Deep service with many abstraction layers; expect bigger output |

Nodes cut off by the depth limit are shown in the web UI as **`… max depth`** (grey).
Actual call cycles are shown as **`↻ cycle`** (red). These are two distinct things.

---

## How it works

```
packages.Load(./...)
    │
    ▼
SSA build  (golang.org/x/tools/go/ssa)
    │
    ▼
Call graph  CHA → VTA refinement
    │
    ▼
Entry point detection
  HTTP  — Gin / Echo / Chi / net/http route registrations
  Kafka — sarama / kafka-go / confluent consumer patterns
  gRPC  — RegisterXxxServer patterns
  Cron  — robfig/cron, go-co-op/gocron
    │
    ▼
Channel consumer index  (ssautil.AllFunctions scan)
  maps chan-element-type → []*ssa.Function that receive from it
    │
    ▼
Per-entry-point DFS traversal (max depth 15)
  • Extracts metadata from SSA instructions at each node
  • Adds synthetic "via=channel" edges from sends to consumers
  • Marks deferred call edges
  • Distinguishes depth cutoff from call cycles in truncated nodes
    │
    ▼
Shared resource analysis  (concurrency conflict detection)
    │
    ▼
JSON output
  ├── web/index.html  (visual explorer)
  └── review CLI      (Claude AI review)
```

---

## Repository layout

```
cmd/
  callgraph/     CLI — build call graph, write JSON
  review/        CLI — interactive AI review via Claude

internal/
  graph/
    builder.go       SSA loader + CHA→VTA call graph construction
    traversal.go     DFS traversal with metadata extraction
    metadata.go      SSA instruction → DBCall / TxOp / HTTPCall / …
    concurrency.go   Shared resource analyser
    serializer.go    JSON serialisation
    types.go         Shared types (CallNode, EntryPoint, …)
    channel_link.go  Channel send → consumer edge builder
    entrypoints/
      detector.go    Entry point finder orchestrator
      http.go        HTTP handler detection (Gin/Echo/Chi/stdlib)
      kafka.go       Kafka consumer detection
      grpc.go        gRPC handler detection
      cron.go        Cron job detection
  review/
    claude.go        Anthropic API client (raw HTTP)
    prompt.go        Prompt builder (system + user message)

web/
  index.html     Single-file interactive visualiser

checks.md        Checklist placed in the analysed project's root (not bundled here)
                 review warns and continues with generic checks when absent
```
