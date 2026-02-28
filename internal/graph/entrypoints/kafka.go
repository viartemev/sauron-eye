package entrypoints

import (
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// kafkaConsumerPatterns lists qualified method names that indicate a Kafka consumer.
var kafkaConsumerPatterns = map[string]bool{
	// Shopify/sarama
	"(*github.com/Shopify/sarama.ConsumerGroup).Consume": true,
	// IBM/sarama (new name)
	"(*github.com/IBM/sarama.ConsumerGroup).Consume": true,
	// confluent-kafka-go
	"(*github.com/confluentinc/confluent-kafka-go/kafka.Consumer).Poll":         true,
	"(*github.com/confluentinc/confluent-kafka-go/v2/kafka.Consumer).Poll":      true,
	// segmentio/kafka-go
	"(*github.com/segmentio/kafka-go.Reader).ReadMessage":  true,
	"(*github.com/segmentio/kafka-go.Reader).FetchMessage": true,
}

// kafkaProtoHandlerPrefixes matches functions that wrap a business handler
// in a proto-decode adapter before passing it to a kafka consumer loop.
// We unwrap these to expose the real handler function as the entry point.
// Pattern: SomeProtoWrapper(businessHandler) → closure calling businessHandler(ctx, *proto.T)
var kafkaProtoHandlerPrefixes = []string{
	"ProtoHandler",   // khandler.ProtoHandler[T](fn)
	"ProtoConsumer",  // common variant name
	"MessageHandler", // common variant name
}

// FindKafkaConsumers scans for Kafka consumer registrations and ConsumeClaim implementations.
func FindKafkaConsumers(prog *ssa.Program, allFuncs []*ssa.Function, fset *token.FileSet, moduleName string) []DetectedEntry {
	var entries []DetectedEntry

	// 1. Find ConsumeClaim implementations (sarama consumer group handler interface)
	for _, fn := range allFuncs {
		if fn.Name() == "ConsumeClaim" && isKafkaConsumeClaim(fn) {
			file, line := funcPosition(fn, fset)
			entries = append(entries, DetectedEntry{
				Source:  "kafka",
				Handler: fn,
				File:    file,
				Line:    line,
			})
		}
	}

	// 2. Find ProtoHandler-style wrapper calls: ProtoHandler(businessFn)
	// These appear in projects that use a custom Kafka consumer loop with
	// proto-decoding adapters (e.g. khandler.ProtoHandler).
	// We extract the inner business handler function as the real entry point.
	for _, fn := range allFuncs {
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				callInstr, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				call := callInstr.Common()
				callee := call.StaticCallee()
				if callee == nil {
					continue
				}
				if !isProtoHandlerWrapper(callee) {
					continue
				}
				// The business handler is always the first (and only real) argument.
				// Generic instantiation: ProtoHandler[T, P](fn) — fn is args[0].
				if len(call.Args) == 0 {
					continue
				}
				inner := extractFunction(call.Args[0])
				if inner == nil {
					continue
				}
				file, line := funcPosition(inner, fset)
				if file == "" {
					if pos := instr.Pos(); pos.IsValid() {
						p := fset.Position(pos)
						file, line = p.Filename, p.Line
					}
				}
				entries = append(entries, DetectedEntry{
					Source:  "kafka",
					Handler: inner,
					File:    file,
					Line:    line,
				})
			}
		}
	}

	// 3. Find explicit Consume / Poll / ReadMessage calls
	detectedConsumers := map[*ssa.Function]bool{}
	for _, fn := range allFuncs {
		// Skip functions that belong to a type which itself implements the kafka Reader
		// interface (has FetchMessage or ReadMessage). Such types are decorators/wrappers
		// around the real reader (e.g. SafeReader, Dedup), not consumer entry points.
		if isKafkaReaderImplementor(fn) {
			continue
		}

		// Skip functions outside the project module to avoid false positives from
		// external libraries (e.g. kafka-go internals, net/http2).
		if moduleName != "" && fn.Package() != nil {
			if !strings.HasPrefix(fn.Package().Pkg.Path(), moduleName) {
				continue
			}
		}

		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				callInstr, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}

				call := callInstr.Common()
				qualName := calleeQualName(call)
				if !kafkaConsumerPatterns[qualName] {
					// Also match calls whose method name is in the kafka pattern set but on a
					// different receiver type — e.g. generated consumer wrappers that proxy to
					// the real kafka.Reader internally ((*GeneratedConsumer).FetchMessage).
					dot := strings.LastIndex(qualName, ".")
					if dot < 0 || !kafkaPatternMethods[qualName[dot+1:]] {
						continue
					}
				}

				file := ""
				line := 0
				if pos := instr.Pos(); pos.IsValid() {
					p := fset.Position(pos)
					file = p.Filename
					line = p.Line
				}

				// Try to extract topic from the args (for sarama Consume: args = [ctx, topics, handler])
				// topics is []string — try to extract from a slice literal
				topic := ""
				args := call.Args
				if strings.Contains(qualName, ".Consume") && len(args) >= 2 {
					topic = extractTopicFromArg(args[1])
				}

				// The handler (ConsumerGroupHandler) for sarama is the last arg
				var handler *ssa.Function
				if strings.Contains(qualName, ".Consume") && len(args) >= 3 {
					handler = extractFunction(args[len(args)-1])
				}

				// For ReadMessage/FetchMessage, the current function IS the handler
				if handler == nil {
					handler = fn
				}

				entries = append(entries, DetectedEntry{
					Source:  "kafka",
					Handler: handler,
					Topic:   topic,
					File:    file,
					Line:    line,
				})

				// For ReadMessage/FetchMessage consumers: also detect any message handler
				// implementations that the consumer loop dispatches to via an interface.
				// Handles the pattern where a project wraps kafka reading in a loop and
				// delegates to an injected handler interface, e.g.:
				//   processBatch → c.handler.HandleBatch(ctx, msgs)
				// Both the consumer loop function and the concrete handler method are entry points.
				if !detectedConsumers[fn] {
					detectedConsumers[fn] = true
					entries = append(entries, findHandlerImpls(fn, allFuncs, fset, moduleName)...)
				}
			}
		}
	}

	return deduplicateKafka(entries)
}

// isKafkaConsumeClaim checks if fn has the signature of sarama's ConsumerGroupHandler.ConsumeClaim.
// Expected: func(session ConsumerGroupSession, claim ConsumerGroupClaim) error
func isKafkaConsumeClaim(fn *ssa.Function) bool {
	sig := fn.Signature
	if sig.Params().Len() < 2 {
		return false
	}
	// Check that parameter types contain sarama-related types
	for i := 0; i < sig.Params().Len(); i++ {
		typeName := sig.Params().At(i).Type().String()
		if strings.Contains(typeName, "sarama") {
			return true
		}
	}
	return false
}

// extractTopicFromArg tries to extract a topic string from a []string slice literal.
// Sarama's Consume takes topics as []string{"my-topic"}, which in SSA becomes:
//
//	t0 = local [1]string          (Alloc)
//	t1 = &t0[0:int]               (IndexAddr)
//	*t1 = "my-topic"              (Store)
//	t2 = t0[0:1]                  (Slice)
//
// We trace the Alloc's referrers to find IndexAddr+Store pairs.
func extractTopicFromArg(val ssa.Value) string {
	// Unwrap Slice to its backing array Alloc.
	if s, ok := val.(*ssa.Slice); ok {
		val = s.X
	}

	alloc, ok := val.(*ssa.Alloc)
	if !ok {
		return extractStringConst(val)
	}

	if alloc.Referrers() == nil {
		return ""
	}
	for _, ref := range *alloc.Referrers() {
		ia, ok := ref.(*ssa.IndexAddr)
		if !ok {
			continue
		}
		if ia.Referrers() == nil {
			continue
		}
		for _, r := range *ia.Referrers() {
			st, ok := r.(*ssa.Store)
			if !ok {
				continue
			}
			if s := extractStringConst(st.Val); s != "" {
				return s
			}
		}
	}
	return ""
}

// funcPosition returns the file and line for a function's definition.
func funcPosition(fn *ssa.Function, fset *token.FileSet) (string, int) {
	if fn.Pos().IsValid() {
		p := fset.Position(fn.Pos())
		return p.Filename, p.Line
	}
	return "", 0
}

// isProtoHandlerWrapper returns true if fn looks like a proto-decoding adapter
// (e.g. khandler.ProtoHandler) that wraps a real business handler.
//
// Matching rules:
//  1. Function name must start with one of kafkaProtoHandlerPrefixes.
//  2. For the generic "MessageHandler" prefix (which is very common), we
//     additionally require that the declaring package path contains a
//     kafka-related keyword to avoid matching unrelated handlers.
func isProtoHandlerWrapper(fn *ssa.Function) bool {
	name := fn.Name()
	for _, prefix := range kafkaProtoHandlerPrefixes {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// "MessageHandler" is too generic on its own; require a kafka package.
		if prefix == "MessageHandler" {
			if fn.Package() == nil {
				return false
			}
			pkgPath := strings.ToLower(fn.Package().Pkg.Path())
			if !strings.Contains(pkgPath, "kafka") &&
				!strings.Contains(pkgPath, "consumer") &&
				!strings.Contains(pkgPath, "handler") {
				return false
			}
		}
		return true
	}
	return false
}

// kafkaPatternMethods is the set of method names extracted from kafkaConsumerPatterns.
// Any type that has one of these methods is a kafka consumer interface implementor
// (a wrapper/decorator), not a consumer entry point.
// Built once at init so adding a new pattern automatically updates the filter.
var kafkaPatternMethods = func() map[string]bool {
	names := make(map[string]bool)
	for pattern := range kafkaConsumerPatterns {
		// Pattern format: "(*pkg/path.Type).MethodName"
		if dot := strings.LastIndex(pattern, "."); dot >= 0 {
			names[pattern[dot+1:]] = true
		}
	}
	return names
}()

// isKafkaReaderImplementor reports whether fn belongs to a type that itself
// implements one of the kafka consumer interfaces (has any method from
// kafkaConsumerPatterns). Such types are decorators/wrappers around the real
// kafka reader — any call they make to the underlying reader is not an entry point.
func isKafkaReaderImplementor(fn *ssa.Function) bool {
	recv := fn.Signature.Recv()
	if recv == nil {
		return false
	}
	T := recv.Type()
	if ptr, ok := T.(*types.Pointer); ok {
		T = ptr.Elem()
	}
	named, ok := T.(*types.Named)
	if !ok {
		return false
	}
	for i := 0; i < named.NumMethods(); i++ {
		if kafkaPatternMethods[named.Method(i).Name()] {
			return true
		}
	}
	return false
}

// deduplicateKafka removes duplicate entries (same handler function).
func deduplicateKafka(entries []DetectedEntry) []DetectedEntry {
	seen := map[*ssa.Function]bool{}
	result := entries[:0]
	for _, e := range entries {
		if !seen[e.Handler] {
			seen[e.Handler] = true
			result = append(result, e)
		}
	}
	return result
}

// findHandlerImpls detects message handler implementations that a Kafka consumer
// loop dispatches to via an interface. It scans fn and its direct static callees
// (within the module) for interface dispatch calls where:
//   - the method name starts with "Handle" or "Process"
//   - the interface type name contains "handler", "consumer", or "processor"
//
// For each such dispatch it finds all concrete implementations of that interface
// method declared within the module and returns them as additional Kafka entry points.
//
// Example: given processBatch → c.handler.HandleBatch(ctx, msgs) where
// c.handler is kafkahandler.Handler, this will return (*handler).HandleBatch.
func findHandlerImpls(fn *ssa.Function, allFuncs []*ssa.Function, fset *token.FileSet, moduleName string) []DetectedEntry {
	toScan := collectDirectCallees(fn, moduleName)

	seen := map[*ssa.Function]bool{}
	var entries []DetectedEntry

	for _, f := range toScan {
		for _, block := range f.Blocks {
			for _, instr := range block.Instrs {
				callInstr, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				call := callInstr.Common()
				if !call.IsInvoke() {
					continue
				}
				if !isHandlerDispatch(call.Method.Name(), call.Value.Type()) {
					continue
				}
				iface, ok := call.Value.Type().Underlying().(*types.Interface)
				if !ok {
					continue
				}
				methodName := call.Method.Name()
				for _, candidate := range allFuncs {
					if candidate.Name() != methodName {
						continue
					}
					if candidate.Package() == nil {
						continue
					}
					if !strings.HasPrefix(candidate.Package().Pkg.Path(), moduleName) {
						continue
					}
					recv := candidate.Signature.Recv()
					if recv == nil {
						continue
					}
					recvType := recv.Type()
					if !types.Implements(recvType, iface) {
						if named, ok2 := recvType.(*types.Named); ok2 {
							if !types.Implements(types.NewPointer(named), iface) {
								continue
							}
						} else {
							continue
						}
					}
					if seen[candidate] {
						continue
					}
					seen[candidate] = true
					file, line := funcPosition(candidate, fset)
					entries = append(entries, DetectedEntry{
						Source:  "kafka",
						Handler: candidate,
						File:    file,
						Line:    line,
					})
				}
			}
		}
	}
	return entries
}

// collectDirectCallees returns fn plus all functions that fn calls directly via
// static (non-interface) calls within the same module. This gives a one-level
// lookahead so that handler dispatches buried one call deep are still detected.
func collectDirectCallees(fn *ssa.Function, moduleName string) []*ssa.Function {
	result := []*ssa.Function{fn}
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			callInstr, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}
			call := callInstr.Common()
			if call.IsInvoke() {
				continue
			}
			callee := call.StaticCallee()
			if callee == nil || callee.Package() == nil {
				continue
			}
			if !strings.HasPrefix(callee.Package().Pkg.Path(), moduleName) {
				continue
			}
			result = append(result, callee)
		}
	}
	return result
}

// isHandlerDispatch reports whether an interface method call looks like a Kafka
// message handler dispatch: the method name starts with "Handle" or "Process",
// and the interface type name contains "handler", "consumer", or "processor".
func isHandlerDispatch(methodName string, ifaceType types.Type) bool {
	if !strings.HasPrefix(methodName, "Handle") && !strings.HasPrefix(methodName, "Process") {
		return false
	}
	typeName := strings.ToLower(ifaceType.String())
	return strings.Contains(typeName, "handler") ||
		strings.Contains(typeName, "consumer") ||
		strings.Contains(typeName, "processor")
}
