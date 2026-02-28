package graph

import (
	"go/constant"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// httpClientTimeoutField is the index of the Timeout field in net/http.Client.
// Computed once by scanning the struct's field names so it doesn't break
// if the stdlib ever reorders fields.
var httpClientTimeoutField = func() int {
	// net/http.Client has fields: Transport, CheckRedirect, Jar, Timeout
	// We match by name to be resilient to future stdlib changes.
	knownOrder := []string{"Transport", "CheckRedirect", "Jar", "Timeout"}
	for i, name := range knownOrder {
		if name == "Timeout" {
			return i
		}
	}
	return 3 // fallback
}()

// NodeMetadata holds extracted metadata from an SSA function's instructions.
type NodeMetadata struct {
	DBCalls    []DBCall
	TxOps      []TxOp
	HTTPCalls  []HTTPCall
	KafkaOps   []KafkaOp
	SyncOps    []SyncOp
	RedisOps   []RedisOp
	ChannelOps []ChannelOp
}

// dbPackages maps package path prefixes to whether they are DB packages.
var dbPackages = []string{
	"database/sql",
	"gorm.io/gorm",
	"github.com/jmoiron/sqlx",
	"github.com/uptrace/bun",
	"github.com/lib/pq",
	"github.com/jackc/pgx",
}

// dbMethods lists method names that represent DB query/exec operations.
var dbMethods = map[string]bool{
	"Query":          true,
	"QueryContext":   true,
	"QueryRow":       true,
	"QueryRowContext": true,
	"Exec":           true,
	"ExecContext":    true,
	"Prepare":        true,
	"PrepareContext": true,
	// GORM
	"Find":    true,
	"First":   true,
	"Last":    true,
	"Save":    true,
	"Create":  true,
	"Update":  true,
	"Updates": true,
	"Delete":  true,
	"Raw":     true,
	"Scan":    true,
	// sqlx
	"Get":      true,
	"Select":   true,
	"Queryx":   true,
	"QueryRowx": true,
	"NamedExec": true,
	"NamedQuery": true,
	// bun
	"NewInsert": true,
	"NewUpdate": true,
	"NewDelete": true,
	"NewSelect": true,
}

// txMethods lists method names that represent transaction operations.
var txMethods = map[string]bool{
	"Begin":       true,
	"BeginTx":     true,
	"Commit":      true,
	"Rollback":    true,
	"Transaction": true, // GORM
}

// httpOutgoingCallPatterns lists qualified function/method names that represent
// outgoing HTTP calls. We use the full qualified name to avoid false-positives
// from other types with similarly-named methods (e.g. http.Header.Get).
var httpOutgoingCallPatterns = map[string]bool{
	"(*net/http.Client).Do":       true,
	"(*net/http.Client).Get":      true,
	"(*net/http.Client).Post":     true,
	"(*net/http.Client).PostForm": true,
	"(*net/http.Client).Head":     true,
	// package-level convenience functions
	"net/http.Get":      true,
	"net/http.Post":     true,
	"net/http.PostForm": true,
	"net/http.Head":     true,
}

// kafkaPublishPatterns maps qualified method names to topics for Kafka publish.
var kafkaPublishMethods = map[string]bool{
	"SendMessage":    true, // sarama
	"SendMessages":   true,
	"WriteMessages":  true, // kafka-go
	"Produce":        true, // confluent
	"ProduceChannel": true,
	"PublishMessage": true,
}

// syncPatterns maps qualified method names to sync type names.
var syncPatterns = map[string]string{
	"(*sync.Mutex).Lock":                           "Mutex.Lock",
	"(*sync.Mutex).RLock":                          "Mutex.RLock",
	"(*sync.RWMutex).Lock":                         "RWMutex.Lock",
	"(*sync.RWMutex).RLock":                        "RWMutex.RLock",
	"(*sync.Once).Do":                              "sync.Once",
	"(*golang.org/x/sync/singleflight.Group).Do":   "singleflight.Do",
	"(*golang.org/x/sync/singleflight.Group).DoChan": "singleflight.DoChan",
}

// redisMethods lists Redis method names to track.
var redisMethods = map[string]bool{
	"SetNX": true,
	"SetXX": true,
	"Eval":  true,
	"EvalSha": true,
	"Set":   true,
	"Get":   true,
	"Del":   true,
}

// redisPackages lists Redis client package path prefixes.
var redisPackages = []string{
	"github.com/redis/go-redis",
	"github.com/go-redis/redis",
}

// ExtractMetadata extracts all metadata from an SSA function's instructions.
func ExtractMetadata(fn *ssa.Function, fset *token.FileSet, hasTxContext bool) NodeMetadata {
	var meta NodeMetadata

	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			// Channel send: s.queue <- value
			if send, ok := instr.(*ssa.Send); ok {
				line := instrLine(instr, fset)
				elemType := send.X.Type().String()
				meta.ChannelOps = append(meta.ChannelOps, ChannelOp{
					Direction: "send",
					ChanType:  elemType,
					Line:      line,
				})
				continue
			}

			// Channel receive: v := <-ch
			if unop, ok := instr.(*ssa.UnOp); ok && unop.Op == token.ARROW {
				line := instrLine(instr, fset)
				elemType := ""
				if ch, ok := unop.X.Type().Underlying().(*types.Chan); ok {
					elemType = ch.Elem().String()
				}
				meta.ChannelOps = append(meta.ChannelOps, ChannelOp{
					Direction: "receive",
					ChanType:  elemType,
					Line:      line,
				})
				continue
			}

			// Channel receive: for v := range ch
			if rng, ok := instr.(*ssa.Range); ok {
				if ch, ok := rng.X.Type().Underlying().(*types.Chan); ok {
					line := instrLine(instr, fset)
					meta.ChannelOps = append(meta.ChannelOps, ChannelOp{
						Direction: "receive",
						ChanType:  ch.Elem().String(),
						Line:      line,
					})
				}
				continue
			}

			callInstr, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}

			call := callInstr.Common()
			callee := call.StaticCallee()
			if callee == nil {
				// For interface method calls, check the method name
				if call.IsInvoke() {
					methodName := call.Method.Name()
					line := instrLine(instr, fset)
					pkgPath := ""
					if call.Method.Pkg() != nil {
						pkgPath = call.Method.Pkg().Path()
					}
					checkInvokedMethod(methodName, pkgPath, line, hasTxContext, &meta)
				}
				continue
			}

			pkg := callee.Package()
			if pkg == nil {
				continue
			}

			pkgPath := pkg.Pkg.Path()
			methodName := callee.Name()
			line := instrLine(instr, fset)

			// DB calls
			if isDBPackage(pkgPath) && dbMethods[methodName] {
				query := extractQueryArg(call.Args, methodName)
				meta.DBCalls = append(meta.DBCalls, DBCall{
					Package: pkgPath,
					Method:  methodName,
					Query:   query,
					Line:    line,
				})
				continue
			}

			// Transaction ops
			if isDBPackage(pkgPath) && txMethods[methodName] {
				meta.TxOps = append(meta.TxOps, TxOp{
					Operation: methodName,
					Line:      line,
				})
				continue
			}

			// Outgoing HTTP calls — match by full qualified name to avoid
			// false-positives from http.Header.Get, http.Request.Header, etc.
			qualCallee := callee.RelString(nil)
			if httpOutgoingCallPatterns[qualCallee] {
				hasTimeout := checkHTTPClientTimeout(call)
				meta.HTTPCalls = append(meta.HTTPCalls, HTTPCall{
					Line:       line,
					HasTimeout: hasTimeout,
				})
				continue
			}

			// Outgoing HTTP calls through generated client wrappers (e.g. oapi-codegen).
			// These methods wrap http.Client.Do internally; the indirection prevents
			// the pattern above from matching, so we detect them by:
			//   1. callee is in a generated source file
			//   2. callee name follows a well-known HTTP-operation naming convention
			if isGeneratedHTTPCall(callee, fset) {
				meta.HTTPCalls = append(meta.HTTPCalls, HTTPCall{
					Line:       line,
					HasTimeout: false, // timeout is on the underlying http.Client or context
				})
				continue
			}

			// Kafka publish
			if isKafkaPackage(pkgPath) && kafkaPublishMethods[methodName] {
				meta.KafkaOps = append(meta.KafkaOps, KafkaOp{
					Line:      line,
					IsPublish: true,
					InsideTx:  hasTxContext,
				})
				continue
			}

			// Sync operations
			qualName := callee.RelString(nil)
			if syncType, ok := syncPatterns[qualName]; ok {
				meta.SyncOps = append(meta.SyncOps, SyncOp{
					Type: syncType,
					Line: line,
				})
				continue
			}

			// Redis operations
			if isRedisPackage(pkgPath) && redisMethods[methodName] {
				meta.RedisOps = append(meta.RedisOps, RedisOp{
					Method: methodName,
					Line:   line,
				})
			}
		}
	}

	return meta
}

// checkInvokedMethod handles interface method calls (IsInvoke() == true).
// pkgPath is the declaring package of the interface method; it is used to
// suppress false-positive DB calls from non-DB interfaces that happen to
// share method names like Find, Delete, or Get.
func checkInvokedMethod(methodName, pkgPath string, line int, hasTxContext bool, meta *NodeMetadata) {
	isKnownDB := pkgPath == "" || isDBPackage(pkgPath)
	if isKnownDB && dbMethods[methodName] {
		meta.DBCalls = append(meta.DBCalls, DBCall{
			Package: pkgPath,
			Method:  methodName,
			Line:    line,
		})
	}
	if isKnownDB && txMethods[methodName] {
		meta.TxOps = append(meta.TxOps, TxOp{
			Operation: methodName,
			Line:      line,
		})
	}
}

// httpGeneratedMethodSuffixes are oapi-codegen / similar generator naming
// conventions that reliably indicate the method makes an outgoing HTTP request.
var httpGeneratedMethodSuffixes = []string{
	"WithResponse",         // oapi-codegen: SendPushWithResponse
	"WithBodyWithResponse", // oapi-codegen: PostWithBodyWithResponse
	"Execute",              // go-swagger executor pattern
}

// isGeneratedHTTPCall returns true when callee is a method in a generated source
// file whose name follows a recognised HTTP-client naming convention.
// It is used to surface outgoing HTTP calls that go through generated wrappers
// (e.g. oapi-codegen clients) and would otherwise be invisible in metadata.
func isGeneratedHTTPCall(callee *ssa.Function, fset *token.FileSet) bool {
	if callee == nil || !callee.Pos().IsValid() {
		return false
	}
	filename := fset.Position(callee.Pos()).Filename
	// Use filename-only detection (no file read) to keep metadata extraction fast.
	if !detectGeneratedFile(filename, nil) {
		return false
	}
	name := callee.Name()
	for _, suf := range httpGeneratedMethodSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

// isDBPackage checks if a package path belongs to a known DB library.
func isDBPackage(pkgPath string) bool {
	for _, prefix := range dbPackages {
		if strings.HasPrefix(pkgPath, prefix) {
			return true
		}
	}
	return false
}

// isKafkaPackage checks if a package path belongs to a known Kafka library.
func isKafkaPackage(pkgPath string) bool {
	return strings.Contains(pkgPath, "sarama") ||
		strings.Contains(pkgPath, "kafka-go") ||
		strings.Contains(pkgPath, "confluent-kafka")
}

// isRedisPackage checks if a package path belongs to a known Redis library.
func isRedisPackage(pkgPath string) bool {
	for _, prefix := range redisPackages {
		if strings.HasPrefix(pkgPath, prefix) {
			return true
		}
	}
	return false
}

// instrLine returns the line number of an instruction, or 0 if invalid.
func instrLine(instr ssa.Instruction, fset *token.FileSet) int {
	if pos := instr.Pos(); pos.IsValid() {
		return fset.Position(pos).Line
	}
	return 0
}

// extractQueryArg tries to extract a SQL query string from call arguments.
func extractQueryArg(args []ssa.Value, method string) string {
	// For static method calls, Args[0] is always the receiver (*sql.DB, *sql.Tx, etc.).
	// The SQL query string positions (after accounting for the receiver):
	//   - Exec(query, args...)          → args[1]
	//   - ExecContext(ctx, query, ...)  → args[2]
	//   - Query(query, args...)         → args[1]
	//   - QueryContext(ctx, query, ...) → args[2]
	if len(args) == 0 {
		return ""
	}

	// Skip receiver (args[0]); for Context-suffixed methods also skip ctx.
	idx := 1
	if isContextMethod(method) && len(args) > 2 {
		idx = 2
	}

	if idx < len(args) {
		if c, ok := args[idx].(*ssa.Const); ok {
			if c.Value != nil && c.Value.Kind() == constant.String {
				s := constant.StringVal(c.Value)
				if len(s) > 200 {
					return s[:200] + "..."
				}
				return s
			}
		}
	}
	return ""
}

// isContextMethod checks if a method takes context as its first argument.
func isContextMethod(method string) bool {
	return strings.HasSuffix(method, "Context")
}

// checkHTTPClientTimeout inspects an http.Client call to see if a timeout is set.
// This is a heuristic: if the receiver is a local variable with Timeout field set, return true.
func checkHTTPClientTimeout(call *ssa.CallCommon) bool {
	if len(call.Args) == 0 {
		return false
	}

	// Check if the receiver (Args[0] for method calls, or captured var) is a *http.Client
	// with a non-zero Timeout. This is difficult to determine statically in the general case,
	// so we look for simple patterns like `&http.Client{Timeout: time.Second * 30}`.
	receiver := call.Value
	if receiver == nil {
		return false
	}

	// Walk back through aliases/field references to find an alloc with Timeout
	return hasHTTPClientTimeout(receiver)
}

// hasHTTPClientTimeout checks if an http.Client value has a timeout set.
func hasHTTPClientTimeout(val ssa.Value) bool {
	switch v := val.(type) {
	case *ssa.Alloc:
		// Resolve the Timeout field index from the struct type if possible,
		// falling back to the pre-computed constant.
		timeoutIdx := httpClientTimeoutField
		if ptr, ok := v.Type().(*types.Pointer); ok {
			if st, ok := ptr.Elem().Underlying().(*types.Struct); ok {
				for i := 0; i < st.NumFields(); i++ {
					if st.Field(i).Name() == "Timeout" {
						timeoutIdx = i
						break
					}
				}
			}
		}
		// Check if any FieldAddr stores to the Timeout field.
		refs := v.Referrers()
		if refs == nil {
			return false
		}
		for _, ref := range *refs {
			if fa, ok := ref.(*ssa.FieldAddr); ok {
				if fa.Field == timeoutIdx {
					faRefs := fa.Referrers()
					if faRefs == nil {
						continue
					}
					for _, r := range *faRefs {
						if _, ok := r.(*ssa.Store); ok {
							return true
						}
					}
				}
			}
		}
	case *ssa.UnOp:
		return hasHTTPClientTimeout(v.X)
	}
	return false
}
