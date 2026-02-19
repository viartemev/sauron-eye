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

				entries = append(entries, DetectedEntry{
					Source:  "http",
					Method:  meta.Method,
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
