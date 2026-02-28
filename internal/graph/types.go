package graph

// EntryPointSource represents the source type of an entry point.
type EntryPointSource string

const (
	SourceHTTP  EntryPointSource = "http"
	SourceKafka EntryPointSource = "kafka"
	SourceGRPC  EntryPointSource = "grpc"
	SourceCron  EntryPointSource = "cron"
)

// NodeCategory classifies what a node in the call graph does.
type NodeCategory string

const (
	CategoryHandler    NodeCategory = "handler"
	CategoryService    NodeCategory = "service"
	CategoryRepository NodeCategory = "repository"
	CategoryHTTPClient NodeCategory = "http_client"
	CategoryProducer   NodeCategory = "producer"
	CategoryUnknown    NodeCategory = "unknown"
)

// EntryPoint represents a top-level entry into the system.
type EntryPoint struct {
	Source        EntryPointSource `json:"source"`
	Method        string           `json:"method,omitempty"`
	Path          string           `json:"path,omitempty"`
	Framework     string           `json:"framework,omitempty"` // e.g. "echo", "gin", "chi", "stdlib"
	FunctionName  string           `json:"function_name"`
	File          string           `json:"file,omitempty"`
	Line          int              `json:"line,omitempty"`
	Topic         string           `json:"topic,omitempty"`
	ConsumerGroup string           `json:"consumer_group,omitempty"`
	CommitMode    string           `json:"commit_mode,omitempty"`
}

// DBCall represents a database operation.
type DBCall struct {
	Package string `json:"package"`
	Method  string `json:"method"`
	Query   string `json:"query,omitempty"`
	Line    int    `json:"line"`
}

// TxOp represents a transaction operation.
type TxOp struct {
	Operation string `json:"operation"` // Begin, BeginTx, Commit, Rollback, Transaction
	Line      int    `json:"line"`
}

// HTTPCall represents an outgoing HTTP call.
type HTTPCall struct {
	Line       int  `json:"line"`
	HasTimeout bool `json:"has_timeout"`
	IsRetried  bool `json:"is_retried"`
}

// KafkaOp represents a Kafka operation.
type KafkaOp struct {
	Topic     string `json:"topic,omitempty"`
	Line      int    `json:"line"`
	IsPublish bool   `json:"is_publish"`
	InsideTx  bool   `json:"inside_tx"`
}

// SyncOp represents a synchronization operation.
type SyncOp struct {
	Type string `json:"type"` // Mutex.Lock, sync.Once, singleflight.Do
	Line int    `json:"line"`
}

// ChannelOp represents a channel send or receive operation.
type ChannelOp struct {
	Direction string `json:"direction"` // "send" or "receive"
	ChanType  string `json:"chan_type"`  // element type flowing through the channel
	Line      int    `json:"line"`
}

// RedisOp represents a Redis operation.
type RedisOp struct {
	Method string `json:"method"` // SetNX, Eval, ...
	Line   int    `json:"line"`
}

// CallEdge is a directed edge in the call graph.
type CallEdge struct {
	Callee   *CallNode `json:"callee"`
	Line     int       `json:"line"`
	Via      string    `json:"via,omitempty"`      // "channel" for synthetic channel edges
	Deferred bool      `json:"deferred,omitempty"` // true when the call site is a defer statement
}

// CallNode represents a node in the call graph.
type CallNode struct {
	FunctionName string       `json:"function_name"`
	Package      string       `json:"package,omitempty"`
	File         string       `json:"file,omitempty"`
	Line         int          `json:"line,omitempty"`
	Category     NodeCategory `json:"category,omitempty"`
	Generated    bool         `json:"generated,omitempty"` // true when the source file was auto-generated
	SourceCode   string       `json:"source_code,omitempty"`
	DBCalls      []DBCall     `json:"db_calls,omitempty"`
	TxOps        []TxOp       `json:"tx_ops,omitempty"`
	HTTPCalls    []HTTPCall   `json:"http_calls,omitempty"`
	KafkaOps     []KafkaOp    `json:"kafka_ops,omitempty"`
	SyncOps      []SyncOp     `json:"sync_ops,omitempty"`
	RedisOps     []RedisOp    `json:"redis_ops,omitempty"`
	ChannelOps   []ChannelOp  `json:"channel_ops,omitempty"`
	Children     []*CallEdge  `json:"children,omitempty"`
	Truncated    bool         `json:"truncated,omitempty"`
	TruncReason  string       `json:"trunc_reason,omitempty"` // "depth" or "cycle"
}

// CallGraph is a full call graph rooted at an entry point.
type CallGraph struct {
	EntryPoint EntryPoint `json:"entry_point"`
	Root       *CallNode  `json:"root"`
}

// BuildResult is the output of the CallGraphBuilder.
type BuildResult struct {
	Module    string      `json:"module,omitempty"` // Go module name auto-detected from go.mod
	Graphs    []CallGraph `json:"graphs"`
	UsedVTA   bool        `json:"used_vta"` // true = CHA+VTA pipeline succeeded; false = CHA only
	Errors    []string    `json:"errors,omitempty"`
	Conflicts []ConcurrencyConflict `json:"conflicts,omitempty"`
}

// ConcurrencyConflict represents a resource accessed by multiple entry points.
type ConcurrencyConflict struct {
	Resource    string                `json:"resource"`
	EntryPoints []EntryPoint          `json:"entry_points"`
	Protection  ConcurrencyProtection `json:"protection"`
}

// ConcurrencyProtection describes what protection mechanisms are in place.
type ConcurrencyProtection struct {
	HasOptimisticLock   bool `json:"has_optimistic_lock"`
	HasPessimisticLock  bool `json:"has_pessimistic_lock"`
	HasAtomicUpdate     bool `json:"has_atomic_update"`
	HasDistributedLock  bool `json:"has_distributed_lock"`
	HasUniqueConstraint bool `json:"has_unique_constraint"`
	HasIdempotencyKey   bool `json:"has_idempotency_key"`
}
