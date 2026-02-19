package entrypoints

import (
	"go/token"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// frameworkMeta holds metadata about an HTTP framework's route registration method.
type frameworkMeta struct {
	Framework     string
	Method        string
	PathArgIdx    int // index in SSA Args (including receiver) where path string is
	HandlerArgIdx int // index in SSA Args where the handler function is
}

// httpRoutePatterns maps qualified method names to their framework metadata.
// In SSA, Args[0] is the receiver for method calls.
var httpRoutePatterns = map[string]frameworkMeta{
	// Gin — (*RouterGroup).METHOD(path, handlers...)
	"(*github.com/gin-gonic/gin.RouterGroup).GET":    {Framework: "gin", Method: "GET", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/gin-gonic/gin.RouterGroup).POST":   {Framework: "gin", Method: "POST", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/gin-gonic/gin.RouterGroup).PUT":    {Framework: "gin", Method: "PUT", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/gin-gonic/gin.RouterGroup).DELETE": {Framework: "gin", Method: "DELETE", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/gin-gonic/gin.RouterGroup).PATCH":  {Framework: "gin", Method: "PATCH", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/gin-gonic/gin.RouterGroup).HEAD":   {Framework: "gin", Method: "HEAD", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/gin-gonic/gin.RouterGroup).OPTIONS": {Framework: "gin", Method: "OPTIONS", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/gin-gonic/gin.RouterGroup).Any":    {Framework: "gin", Method: "ANY", PathArgIdx: 1, HandlerArgIdx: 2},

	// Echo — (*Echo).METHOD(path, handler, middleware...)
	"(*github.com/labstack/echo/v4.Echo).GET":    {Framework: "echo", Method: "GET", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/labstack/echo/v4.Echo).POST":   {Framework: "echo", Method: "POST", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/labstack/echo/v4.Echo).PUT":    {Framework: "echo", Method: "PUT", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/labstack/echo/v4.Echo).DELETE": {Framework: "echo", Method: "DELETE", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/labstack/echo/v4.Echo).PATCH":  {Framework: "echo", Method: "PATCH", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/labstack/echo/v4.Group).GET":   {Framework: "echo", Method: "GET", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/labstack/echo/v4.Group).POST":  {Framework: "echo", Method: "POST", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/labstack/echo/v4.Group).PUT":   {Framework: "echo", Method: "PUT", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/labstack/echo/v4.Group).DELETE": {Framework: "echo", Method: "DELETE", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/labstack/echo/v4.Group).PATCH":  {Framework: "echo", Method: "PATCH", PathArgIdx: 1, HandlerArgIdx: 2},

	// Chi — (*Mux).Method(pattern, handlerFn)
	"(*github.com/go-chi/chi/v5.Mux).Get":     {Framework: "chi", Method: "GET", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/go-chi/chi/v5.Mux).Post":    {Framework: "chi", Method: "POST", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/go-chi/chi/v5.Mux).Put":     {Framework: "chi", Method: "PUT", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/go-chi/chi/v5.Mux).Delete":  {Framework: "chi", Method: "DELETE", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/go-chi/chi/v5.Mux).Patch":   {Framework: "chi", Method: "PATCH", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/go-chi/chi/v5.Mux).Head":    {Framework: "chi", Method: "HEAD", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/go-chi/chi/v5.Mux).Options": {Framework: "chi", Method: "OPTIONS", PathArgIdx: 1, HandlerArgIdx: 2},

	// gorilla/mux — (*Router).HandleFunc(path, handler)
	// Note: .Methods() is chained after, so HTTP method is unknown here.
	"(*github.com/gorilla/mux.Router).HandleFunc": {Framework: "gorilla", Method: "ANY", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*github.com/gorilla/mux.Router).Handle":     {Framework: "gorilla", Method: "ANY", PathArgIdx: 1, HandlerArgIdx: 2},
	// gorilla/mux subrouter (same underlying type)
	"(*github.com/gorilla/mux.Route).HandlerFunc": {Framework: "gorilla", Method: "ANY", PathArgIdx: 1, HandlerArgIdx: 2},

	// stdlib net/http — HandleFunc(pattern, handler)
	"net/http.HandleFunc":               {Framework: "stdlib", Method: "ANY", PathArgIdx: 0, HandlerArgIdx: 1},
	"(*net/http.ServeMux).HandleFunc":   {Framework: "stdlib", Method: "ANY", PathArgIdx: 1, HandlerArgIdx: 2},
	"(*net/http.ServeMux).Handle":       {Framework: "stdlib", Method: "ANY", PathArgIdx: 1, HandlerArgIdx: 2},
}

// FindHTTPHandlers scans all functions for HTTP route registration calls and returns the handlers.
// moduleName, when non-empty, restricts the scan to functions whose package path starts with
// that prefix. This prevents third-party packages (expvar, go-restful, trace, …) that internally
// call net/http.Handle/HandleFunc from being mistaken for project route registrations.
func FindHTTPHandlers(prog *ssa.Program, allFuncs []*ssa.Function, fset *token.FileSet, moduleName string) []DetectedEntry {
	var entries []DetectedEntry

	// Build an inter-procedural index so we can resolve prefixes when a
	// subrouter is passed as a function parameter rather than used inline.
	subrouterIndex := buildGorillaSubrouterIndex(allFuncs)

	for _, fn := range allFuncs {
		// Only scan project code for route registrations.
		if moduleName != "" {
			pkg := fn.Package()
			if pkg == nil || !strings.HasPrefix(pkg.Pkg.Path(), moduleName) {
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
				meta, matched := httpRoutePatterns[qualName]
				if !matched {
					continue
				}

				args := call.Args
				if len(args) <= meta.HandlerArgIdx {
					continue
				}

				handler := extractFunction(args[meta.HandlerArgIdx])
				if handler == nil {
					// Try remaining args (variadic handlers)
					for i := meta.HandlerArgIdx; i < len(args); i++ {
						if h := extractFunction(args[i]); h != nil {
							handler = h
							break
						}
					}
				}
				if handler == nil {
					continue
				}

				path := ""
				if meta.PathArgIdx < len(args) {
					path = extractStringConst(args[meta.PathArgIdx])
				}

				file := ""
				line := 0
				if pos := instr.Pos(); pos.IsValid() {
					p := fset.Position(pos)
					file = p.Filename
					line = p.Line
				}

				method := meta.Method
				if meta.Framework == "gorilla" {
					// Resolve the subrouter prefix chain (PathPrefix + Subrouter),
					// including the case where the router was passed as a parameter.
					if len(args) > 0 {
						if prefix := resolveGorillaPrefixFull(args[0], subrouterIndex); prefix != "" {
							path = prefix + path
						}
					}
					// Resolve the HTTP method from a chained .Methods() call.
					if callVal, ok := callInstr.(ssa.Value); ok {
						if m := extractGorillaMethod(callVal); m != "" {
							method = m
						}
					}
				}

				entries = append(entries, DetectedEntry{
					Source:  "http",
					Method:  method,
					Path:    path,
					Handler: handler,
					File:    file,
					Line:    line,
				})
			}
		}
	}

	return entries
}

// resolveGorillaPrefix walks the SSA def-use chain of a gorilla/mux receiver
// to collect any PathPrefix strings from enclosing subrouters.
// This is the inline-only resolver: it works when the subrouter is used
// directly in the same function. For the parameter-passing case see
// resolveGorillaPrefixFull.
//
// gorilla/mux subrouter pattern in SSA:
//
//	t0 = (*mux.Router).PathPrefix(router, "/v2")   → *mux.Route
//	t1 = (*mux.Route).Subrouter(t0)                → *mux.Router
//	t2 = (*mux.Router).HandleFunc(t1, "/path", h)  ← receiver is t1
//
// Given t1 (the receiver of HandleFunc), we walk:
//
//	t1 defined by Subrouter(t0)
//	t0 defined by PathPrefix(parent, "/v2")
//	recurse on parent
func resolveGorillaPrefix(receiver ssa.Value) string {
	callVal, ok := receiver.(*ssa.Call)
	if !ok {
		return ""
	}
	callee := callVal.Call.StaticCallee()
	if callee == nil {
		return ""
	}
	if callee.RelString(nil) != "(*github.com/gorilla/mux.Route).Subrouter" {
		return ""
	}
	// The receiver of Subrouter() is the *mux.Route returned by PathPrefix().
	if len(callVal.Call.Args) == 0 {
		return ""
	}
	routeVal := callVal.Call.Args[0]

	routeCall, ok := routeVal.(*ssa.Call)
	if !ok {
		return ""
	}
	routeCallee := routeCall.Call.StaticCallee()
	if routeCallee == nil {
		return ""
	}
	if routeCallee.RelString(nil) != "(*github.com/gorilla/mux.Router).PathPrefix" {
		return ""
	}
	// Args: [receiver_router, prefix_string]
	if len(routeCall.Call.Args) < 2 {
		return ""
	}
	prefix := extractStringConst(routeCall.Call.Args[1])
	// Recurse: the Router passed to PathPrefix might itself be a subrouter.
	parentPrefix := resolveGorillaPrefix(routeCall.Call.Args[0])
	return parentPrefix + prefix
}

// resolveGorillaPrefixFull extends resolveGorillaPrefix to handle the case
// where the subrouter is passed as a function parameter rather than used inline.
//
// Example:
//
//	func setup(r *mux.Router) {
//	    v2 := r.PathPrefix("/v2").Subrouter()   // *ssa.Call — resolved inline
//	    app.registerHandlers(v2)               // v2 passed as arg
//	}
//	func registerHandlers(v2 *mux.Router) {    // v2 is *ssa.Parameter here
//	    v2.HandleFunc("/message", h)           // receiver is *ssa.Parameter
//	}
//
// subrouterIndex is built by buildGorillaSubrouterIndex.
func resolveGorillaPrefixFull(receiver ssa.Value, subrouterIndex map[*ssa.Function]map[int]string) string {
	// Case 1: inline — the receiver is the direct result of a Subrouter() call.
	if prefix := resolveGorillaPrefix(receiver); prefix != "" {
		return prefix
	}
	// Case 2: parameter — the subrouter was passed in from a call site.
	param, ok := receiver.(*ssa.Parameter)
	if !ok {
		return ""
	}
	fn := param.Parent()
	for i, p := range fn.Params {
		if p == param {
			if prefixes, ok := subrouterIndex[fn]; ok {
				return prefixes[i]
			}
			break
		}
	}
	return ""
}

// buildGorillaSubrouterIndex performs a single pass over all functions and
// records which function parameters carry a gorilla/mux subrouter with a
// known prefix. This enables resolveGorillaPrefixFull to cross function
// call boundaries.
//
// For every call site that passes a subrouter value (one whose inline prefix
// resolves to a non-empty string) as an argument, we record:
//
//	index[callee][argIndex] = prefix
func buildGorillaSubrouterIndex(allFuncs []*ssa.Function) map[*ssa.Function]map[int]string {
	index := make(map[*ssa.Function]map[int]string)
	for _, fn := range allFuncs {
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				callInstr, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				callee := callInstr.Common().StaticCallee()
				if callee == nil {
					continue
				}
				for i, arg := range callInstr.Common().Args {
					prefix := resolveGorillaPrefix(arg)
					if prefix == "" {
						continue
					}
					if index[callee] == nil {
						index[callee] = make(map[int]string)
					}
					index[callee][i] = prefix
				}
			}
		}
	}
	return index
}

// extractGorillaMethod looks for a .Methods(...) call chained on the return
// value of a HandleFunc/Handle call and returns the first HTTP method string.
//
// gorilla/mux pattern:
//
//	route := router.HandleFunc("/path", handler)
//	route.Methods(http.MethodPost)
//
// In SSA the return value of HandleFunc is a *mux.Route; we walk its
// referrers looking for a (*mux.Route).Methods call.
func extractGorillaMethod(result ssa.Value) string {
	refs := result.Referrers()
	if refs == nil {
		return ""
	}
	for _, ref := range *refs {
		callInstr, ok := ref.(ssa.CallInstruction)
		if !ok {
			continue
		}
		call := callInstr.Common()
		if calleeQualName(call) != "(*github.com/gorilla/mux.Route).Methods" {
			continue
		}
		// Args: [receiver *Route, methods ...string]
		// The variadic is compiled as a []string slice — reuse extractTopicFromArg.
		if len(call.Args) < 2 {
			continue
		}
		if m := extractTopicFromArg(call.Args[1]); m != "" {
			return strings.ToUpper(m)
		}
	}
	return ""
}
