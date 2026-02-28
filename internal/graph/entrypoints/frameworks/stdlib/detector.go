// Package stdlib detects HTTP entry points registered via the standard library
// net/http package (http.HandleFunc, ServeMux.HandleFunc, ServeMux.Handle).
//
// Register this detector by importing the package for its side effects:
//
//	import _ "sauron-eye/internal/graph/entrypoints/frameworks/stdlib"
package stdlib

import (
	"go/token"

	"golang.org/x/tools/go/ssa"

	"sauron-eye/internal/graph/entrypoints"
)

func init() {
	entrypoints.RegisterHTTPDetector(&detector{})
}

type detector struct{}

func (d *detector) Name() string { return "stdlib" }

// patterns covers the three net/http route-registration call sites.
// PathArgIdx / HandlerArgIdx are SSA Args indices (receiver is Args[0] for methods).
var patterns = map[string]entrypoints.RoutePattern{
	// http.HandleFunc(pattern, handler) — package-level, no receiver
	"net/http.HandleFunc": {Method: "ANY", PathArgIdx: 0, HandlerArgIdx: 1},
	// (*http.ServeMux).HandleFunc(pattern, handler)
	"(*net/http.ServeMux).HandleFunc": {Method: "ANY", PathArgIdx: 1, HandlerArgIdx: 2},
	// (*http.ServeMux).Handle(pattern, handler)
	"(*net/http.ServeMux).Handle": {Method: "ANY", PathArgIdx: 1, HandlerArgIdx: 2},
}

func (d *detector) Detect(allFuncs []*ssa.Function, fset *token.FileSet, _ string) []entrypoints.DetectedEntry {
	return entrypoints.DetectByRoutePatterns(allFuncs, fset, patterns, "stdlib")
}
