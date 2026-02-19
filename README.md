# Sauron Eye — Call Graph Analyser

Static call-graph builder for Go projects with an interactive web visualiser.
Builds a full SSA call graph (CHA → VTA), detects entry points (HTTP handlers,
Kafka consumers, gRPC handlers, cron jobs), extracts metadata (DB calls,
transactions, HTTP calls, Kafka ops, Redis ops, channel sends/receives, deferred
calls), and serialises everything to JSON for the web UI.

---

## Requirements

| Tool | Version |
|------|---------|
| Go   | 1.22+   |
| A Go project to analyse | any module-based Go project |

No external services needed — everything runs locally.

---

## Quick start

```bash
# 1. Clone and build the analyser
git clone <repo-url> sauron-eye
cd sauron-eye
go build -o callgraph ./cmd/callgraph

# 2. Analyse a project
./callgraph -output graph.json /path/to/your/go/project

# 3. Open the visualiser
open web/index.html          # macOS
xdg-open web/index.html      # Linux
# Then drop graph.json onto the page
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
| `-output` | stdout | File path to write JSON to. If omitted, JSON is written to stdout. |
| `-format` | `pretty` | `pretty` — indented JSON; `json` — compact JSON. |
| `-no-source` | false | Strip source-code snippets from output. Reduces file size significantly; detail panel in the web UI will be empty. |
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
  "graphs": [ ... ],         // one CallGraph per entry point
  "used_pta": true,          // false = fell back to CHA only
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
  "source_code": "func (s *Service) ...",
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
JSON output  →  web/index.html
```
