package entrypoints

import (
	"go/token"
	"strings"
	"sync"

	"golang.org/x/tools/go/ssa"
)

// RoutePattern describes a single route-registration call site.
// PathArgIdx and HandlerArgIdx are indices into the SSA Args slice,
// which includes the receiver at index 0 for method calls.
type RoutePattern struct {
	Method        string // HTTP method: "GET", "POST", "ANY", etc.
	PathArgIdx    int    // SSA Args index of the route-path string argument
	HandlerArgIdx int    // SSA Args index of the handler function argument
}

// HTTPFrameworkDetector is implemented by each HTTP framework package.
// Register implementations via RegisterHTTPDetector (typically in init()).
type HTTPFrameworkDetector interface {
	// Name returns a short human-readable name, e.g. "stdlib", "gin".
	Name() string

	// Detect scans allFuncs for route-registration calls and returns the
	// discovered entry points. allFuncs is already filtered to project-owned
	// packages by FindHTTPHandlers.
	Detect(allFuncs []*ssa.Function, fset *token.FileSet, moduleName string) []DetectedEntry
}

var (
	httpDetectorsMu sync.RWMutex
	httpDetectors   []HTTPFrameworkDetector
)

// RegisterHTTPDetector adds d to the global registry. It is safe to call
// concurrently and is typically invoked from framework package init() functions.
func RegisterHTTPDetector(d HTTPFrameworkDetector) {
	httpDetectorsMu.Lock()
	defer httpDetectorsMu.Unlock()
	httpDetectors = append(httpDetectors, d)
}

// DetectByRoutePatterns is a shared scan loop for simple frameworks that
// follow the "router.METHOD(path, handler)" registration convention.
// It iterates allFuncs, matches CalleeQualName against patterns, and extracts
// path + handler arguments according to each pattern's index configuration.
// framework is recorded on every returned DetectedEntry (e.g. "echo", "stdlib").
func DetectByRoutePatterns(
	allFuncs []*ssa.Function,
	fset *token.FileSet,
	patterns map[string]RoutePattern,
	framework string,
) []DetectedEntry {
	var entries []DetectedEntry

	for _, fn := range allFuncs {
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				callInstr, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}

				call := callInstr.Common()
				meta, matched := patterns[CalleeQualName(call)]
				if !matched {
					continue
				}

				args := call.Args
				if len(args) <= meta.HandlerArgIdx {
					continue
				}

				handler := ExtractFunction(args[meta.HandlerArgIdx])
				if handler == nil {
					// Try remaining args (variadic handlers — take the first found)
					for i := meta.HandlerArgIdx; i < len(args); i++ {
						if h := ExtractFunction(args[i]); h != nil {
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
					path = ExtractStringConst(args[meta.PathArgIdx])
				}

				file := ""
				line := 0
				if pos := instr.Pos(); pos.IsValid() {
					p := fset.Position(pos)
					file = p.Filename
					line = p.Line
				}

				entries = append(entries, DetectedEntry{
					Source:    "http",
					Method:    meta.Method,
					Path:      path,
					Framework: framework,
					Handler:   handler,
					File:      file,
					Line:      line,
				})
			}
		}
	}

	return entries
}

// FindHTTPHandlers calls every registered HTTPFrameworkDetector and returns
// the combined set of discovered HTTP entry points.
// allFuncs is filtered to project-owned packages before being passed to detectors.
func FindHTTPHandlers(prog *ssa.Program, allFuncs []*ssa.Function, fset *token.FileSet, moduleName string) []DetectedEntry {
	projectFuncs := filterToModule(allFuncs, moduleName)

	httpDetectorsMu.RLock()
	detectors := make([]HTTPFrameworkDetector, len(httpDetectors))
	copy(detectors, httpDetectors)
	httpDetectorsMu.RUnlock()

	var entries []DetectedEntry
	for _, d := range detectors {
		entries = append(entries, d.Detect(projectFuncs, fset, moduleName)...)
	}
	return entries
}

// filterToModule returns only functions whose package path starts with moduleName.
// When moduleName is empty all functions are returned unchanged.
func filterToModule(allFuncs []*ssa.Function, moduleName string) []*ssa.Function {
	if moduleName == "" {
		return allFuncs
	}
	out := make([]*ssa.Function, 0, len(allFuncs))
	for _, fn := range allFuncs {
		if pkg := fn.Package(); pkg != nil && strings.HasPrefix(pkg.Pkg.Path(), moduleName) {
			out = append(out, fn)
		}
	}
	return out
}
