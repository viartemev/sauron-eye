// Package entrypoints detects all entry points in an SSA program:
// HTTP handlers, Kafka consumers, gRPC handlers, and cron jobs.
package entrypoints

import (
	"go/constant"
	"go/token"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// DetectedEntry is a discovered entry point before conversion to graph.EntryPoint.
type DetectedEntry struct {
	Source        string // "http", "kafka", "grpc", "cron"
	Method        string // HTTP method or empty
	Path          string // route path or topic name
	Handler       *ssa.Function
	File          string
	Line          int
	Topic         string
	ConsumerGroup string
	CommitMode    string
}

// FindAll discovers all entry points in the given SSA program.
// moduleName restricts HTTP handler scanning to project-owned packages only.
func FindAll(prog *ssa.Program, fset *token.FileSet, moduleName string) []DetectedEntry {
	var entries []DetectedEntry

	// Collect all functions in the program (including anonymous functions)
	allFuncs := collectAllFunctions(prog)

	// Run each detector
	entries = append(entries, FindHTTPHandlers(prog, allFuncs, fset, moduleName)...)
	entries = append(entries, FindKafkaConsumers(prog, allFuncs, fset, moduleName)...)
	entries = append(entries, FindGRPCHandlers(prog, allFuncs, fset)...)
	entries = append(entries, FindCronJobs(prog, allFuncs, fset)...)

	return entries
}

// collectAllFunctions collects ALL functions in the SSA program:
// package-level functions, methods, closures, and anonymous functions.
//
// The previous approach only iterated pkg.Members which excludes methods
// (Go SSA docs: "Methods are not in Members"). ssautil.AllFunctions
// correctly returns the full universe including all method functions.
func collectAllFunctions(prog *ssa.Program) []*ssa.Function {
	all := ssautil.AllFunctions(prog)
	result := make([]*ssa.Function, 0, len(all))
	for fn := range all {
		if fn != nil {
			result = append(result, fn)
		}
	}
	return result
}

// flattenFunctions returns fn and all of its nested anonymous functions.
func flattenFunctions(fn *ssa.Function) []*ssa.Function {
	result := []*ssa.Function{fn}
	for _, anon := range fn.AnonFuncs {
		result = append(result, flattenFunctions(anon)...)
	}
	return result
}

// extractStringConst extracts a string constant from an SSA value, returning empty string if not constant.
func extractStringConst(val ssa.Value) string {
	if c, ok := val.(*ssa.Const); ok {
		// Unquote the string value
		s := c.Value.String()
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			return s[1 : len(s)-1]
		}
		return s
	}
	return ""
}

// extractFunction extracts an *ssa.Function from an SSA value.
// Handles direct function references, closures, type coercions (ChangeType),
// and variadic slices (e.g. gin's ...HandlerFunc).
func extractFunction(val ssa.Value) *ssa.Function {
	switch v := val.(type) {
	case *ssa.Function:
		return v
	case *ssa.MakeClosure:
		if fn, ok := v.Fn.(*ssa.Function); ok {
			return fn
		}
	case *ssa.ChangeType:
		// Type coercion: e.g. changetype gin.HandlerFunc <- func(*gin.Context)
		return extractFunction(v.X)
	case *ssa.MakeInterface:
		// Interface wrapping: unwrap to get underlying function
		return extractFunction(v.X)
	case *ssa.Slice:
		// Variadic call: Go compiles f(a, handler) where f is func(...T) into
		// a slice allocation, store handler at index 0, then pass slice.
		// We extract the function stored at index 0 of the underlying array.
		return extractFunctionFromSlice(v)
	}
	return nil
}

// extractFunctionFromSlice finds the *ssa.Function stored at index 0 of a
// variadic-argument slice (e.g., generated for Gin's ...HandlerFunc).
func extractFunctionFromSlice(s *ssa.Slice) *ssa.Function {
	arr := s.X
	if arr == nil {
		return nil
	}
	refs := arr.Referrers()
	if refs == nil {
		return nil
	}
	for _, ref := range *refs {
		ia, ok := ref.(*ssa.IndexAddr)
		if !ok {
			continue
		}
		// Check that the index is 0 (first handler)
		c, ok := ia.Index.(*ssa.Const)
		if !ok {
			continue
		}
		v, ok := constant.Int64Val(c.Value)
		if !ok || v != 0 {
			continue
		}
		// Find the Store instruction that writes to this address
		storeRefs := ia.Referrers()
		if storeRefs == nil {
			continue
		}
		for _, sr := range *storeRefs {
			store, ok := sr.(*ssa.Store)
			if !ok {
				continue
			}
			if fn := extractFunction(store.Val); fn != nil {
				return fn
			}
		}
	}
	return nil
}

// calleeQualName returns the qualified name of a static callee, or empty string.
func calleeQualName(call *ssa.CallCommon) string {
	if fn := call.StaticCallee(); fn != nil {
		return fn.RelString(nil)
	}
	return ""
}
