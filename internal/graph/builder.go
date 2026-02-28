package graph

import (
	"context"
	"fmt"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"sauron-eye/internal/graph/entrypoints"
)

// Config controls the behaviour of the CallGraphBuilder.
type Config struct {
	// ProjectPath is the root directory of the project to analyse.
	ProjectPath string

	// MaxDepth limits how deep the recursive call-tree traversal goes.
	// Default: 6.
	MaxDepth int

	// PTATimeout is kept for CLI backward-compatibility but is no longer used
	// (PTA was replaced by the CHA→VTA pipeline which doesn't need a timeout).
	PTATimeout interface{}

	// ScopePatterns are Go package patterns passed to packages.Load.
	// Defaults to []string{"./..."}.
	ScopePatterns []string

	// PkgFilter is a comma-separated list of package-path prefixes to include
	// during traversal. Nodes whose package does not match any prefix are
	// recorded in metadata but not recursed into.
	// Auto-populated from go.mod module name when empty.
	PkgFilter string

	// SkipGenerated, when true, prevents the traversal from recursing into
	// auto-generated source files (files with the standard "// Code generated"
	// header or common generated-file name suffixes such as .pb.go, _gen.go).
	// Generated nodes are still recorded in the graph (with Generated=true) but
	// their subtrees are not expanded.
	SkipGenerated bool
}

func (c *Config) setDefaults() {
	if c.MaxDepth == 0 {
		c.MaxDepth = 15
	}
	if len(c.ScopePatterns) == 0 {
		c.ScopePatterns = []string{"./..."}
	}
}

// readModuleName parses the first "module" directive from go.mod.
func readModuleName(projectPath string) string {
	data, err := os.ReadFile(filepath.Join(projectPath, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// CallGraphBuilder builds call graphs for a Go project.
type CallGraphBuilder struct {
	cfg Config
}

// NewCallGraphBuilder creates a new builder with the given config.
func NewCallGraphBuilder(cfg Config) *CallGraphBuilder {
	cfg.setDefaults()
	return &CallGraphBuilder{cfg: cfg}
}

// Build loads the project, constructs the SSA form, and builds call graphs
// for every detected entry point.
func (b *CallGraphBuilder) Build(ctx context.Context) (*BuildResult, error) {
	result := &BuildResult{}

	// Auto-detect module name; use as default pkg filter if none provided.
	module := readModuleName(b.cfg.ProjectPath)
	result.Module = module
	if b.cfg.PkgFilter == "" {
		b.cfg.PkgFilter = module
	}

	// ── 1. Load packages ──────────────────────────────────────────────────
	log.Printf("[builder] loading packages from %s …", b.cfg.ProjectPath)
	loadCfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedImports |
			packages.NeedTypes |
			packages.NeedTypesSizes |
			packages.NeedSyntax |
			packages.NeedTypesInfo |
			packages.NeedDeps,
		Dir:     b.cfg.ProjectPath,
		Context: ctx,
		// Suppress noisy build errors from test files etc.
		Tests: false,
	}

	pkgs, err := packages.Load(loadCfg, b.cfg.ScopePatterns...)
	if err != nil {
		return nil, fmt.Errorf("packages.Load: %w", err)
	}

	// Collect any load-time errors as warnings but continue.
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			result.Errors = append(result.Errors, fmt.Sprintf("load error in %s: %v", pkg.PkgPath, e))
		}
	}

	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages found in %s", b.cfg.ProjectPath)
	}

	// ── 2. Build SSA ──────────────────────────────────────────────────────
	log.Printf("[builder] building SSA for %d packages …", len(pkgs))
	prog, _ := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()

	fset := prog.Fset

	// ── 3. Build call graph: CHA → VTA refinement ────────────────────────
	cg, usedVTA := b.buildCallGraph(prog)
	result.UsedVTA = usedVTA
	log.Printf("[builder] call graph ready (VTA=%v)", usedVTA)

	// ── 4. Detect entry points ────────────────────────────────────────────
	detected := entrypoints.FindAll(prog, fset, module)
	log.Printf("[builder] detected %d entry points", len(detected))

	if len(detected) == 0 {
		result.Errors = append(result.Errors, "no entry points detected (HTTP/Kafka/gRPC/cron handlers)")
	}

	// ── 5. Build per-entry-point call graphs ──────────────────────────────
	consumers := BuildChannelConsumerIndex(prog)
	log.Printf("[builder] channel consumer index: %d element types", len(consumers))

	pkgPrefixes := parsePkgFilter(b.cfg.PkgFilter)
	if len(pkgPrefixes) > 0 {
		log.Printf("[builder] pkg filter: %v", pkgPrefixes)
	}
	traversal := NewTraversal(prog, cg, fset, b.cfg.MaxDepth, consumers, pkgPrefixes, b.cfg.SkipGenerated)

	for _, ep := range detected {
		if ep.Handler == nil {
			continue
		}

		// Unwrap synthetic $bound/$thunk wrappers to the real underlying method.
		// When a method value (e.g. c.MessageClient) is registered as a handler,
		// SSA represents it as a synthetic "$bound" thunk that immediately calls
		// the real method. Using the real method as the root gives a correct
		// function name, proper category, and skips the trivial wrapper node.
		handler := ep.Handler
		if handler.Synthetic != "" {
			if cgNode := cg.Nodes[handler]; cgNode != nil {
				for _, edge := range cgNode.Out {
					if callee := edge.Callee.Func; callee != nil && callee.Synthetic == "" {
						handler = callee
						break
					}
				}
			}
		}

		// Resolve generated adapter chains to the first non-generated handler.
		// Frameworks like oapi-codegen place multiple layers of generated glue
		// between the route registration and the user's business logic:
		//   serverInterfaceWrapper.Refund (generated)
		//     → strictHandler.Refund      (generated)
		//       → UserImpl.Refund         (business logic ← this is what we want)
		// Following through these layers surfaces the real handler as the entry
		// point name/location without losing any traversal depth.
		handlerFile, handlerLine := ep.File, ep.Line
		if real := resolveGeneratedHandler(handler, cg, fset, module); real != handler {
			handler = real
			// Also update the file/line to point at the real handler, not the
			// generated registration site.
			if real.Pos().IsValid() {
				p := fset.Position(real.Pos())
				handlerFile, handlerLine = p.Filename, p.Line
			}
		}

		entryPoint := EntryPoint{
			Source:        EntryPointSource(ep.Source),
			Method:        ep.Method,
			Path:          ep.Path,
			Framework:     ep.Framework,
			FunctionName:  handler.RelString(nil),
			File:          handlerFile,
			Line:          handlerLine,
			Topic:         ep.Topic,
			ConsumerGroup: ep.ConsumerGroup,
			CommitMode:    ep.CommitMode,
		}

		root := traversal.BuildNode(handler, 0, make(visitedSet), false)

		result.Graphs = append(result.Graphs, CallGraph{
			EntryPoint: entryPoint,
			Root:       root,
		})
	}

	// ── 6. Shared resource / concurrency analysis ─────────────────────────
	analyzer := NewSharedResourceAnalyzer()
	result.Conflicts = analyzer.Analyze(result.Graphs)

	return result, nil
}

// buildCallGraph builds a call graph using the CHA→VTA pipeline.
//
// Strategy:
//  1. CHA (Class Hierarchy Analysis) — fast, sound, but imprecise (over-approximates)
//  2. VTA (Variable Type Analysis)   — refines CHA by propagating concrete types
//     through the type-propagation graph; more precise than CHA, no panics
//
// VTA takes the CHA graph as its initial graph and the set of all reachable
// functions, producing a refined graph without the reliability issues of the
// deprecated pointer-analysis package.
func (b *CallGraphBuilder) buildCallGraph(prog *ssa.Program) (cg *callgraph.Graph, usedVTA bool) {
	// Step 1: build a CHA call graph (used both as fallback and VTA seed).
	chaCG := cha.CallGraph(prog)

	// Step 2: collect the set of functions reachable from CHA for VTA.
	allFuncs := make(map[*ssa.Function]bool)
	for fn := range chaCG.Nodes {
		if fn != nil {
			allFuncs[fn] = true
		}
	}

	if len(allFuncs) == 0 {
		log.Printf("[builder] no reachable functions; using raw CHA graph")
		return chaCG, false
	}

	// Step 3: refine with VTA.
	// Named return values allow the deferred recover to set cg=chaCG on panic,
	// so callers always get a valid (non-nil) graph.
	cg, usedVTA = chaCG, false
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[builder] VTA panic (%v); falling back to CHA", r)
			cg, usedVTA = chaCG, false
		}
	}()

	vtaCG := vta.CallGraph(allFuncs, chaCG)
	return vtaCG, true
}

// resolveGeneratedHandler follows the call graph through generated adapter
// layers (e.g. oapi-codegen's serverInterfaceWrapper → strictHandler → user
// impl) and returns the first non-generated callee that belongs to the project
// module. If no such callee is found the original handler is returned unchanged.
func resolveGeneratedHandler(handler *ssa.Function, cg *callgraph.Graph, fset *token.FileSet, module string) *ssa.Function {
	if !isFuncGenerated(handler, fset) {
		return handler
	}

	visited := make(map[*ssa.Function]bool)
	var walk func(fn *ssa.Function) *ssa.Function
	walk = func(fn *ssa.Function) *ssa.Function {
		if visited[fn] {
			return nil
		}
		visited[fn] = true

		node := cg.Nodes[fn]
		if node == nil {
			return nil
		}
		for _, edge := range node.Out {
			callee := edge.Callee.Func
			if callee == nil || callee.Synthetic != "" {
				continue
			}
			// Must be in the project module.
			if pkg := callee.Package(); pkg == nil || !strings.HasPrefix(pkg.Pkg.Path(), module) {
				continue
			}
			if !isFuncGenerated(callee, fset) {
				return callee
			}
			if result := walk(callee); result != nil {
				return result
			}
		}
		return nil
	}

	if result := walk(handler); result != nil {
		return result
	}
	return handler
}

// isFuncGenerated reports whether fn's source file is auto-generated.
// It reuses the detectGeneratedFile logic from traversal.go (same package).
func isFuncGenerated(fn *ssa.Function, fset *token.FileSet) bool {
	if !fn.Pos().IsValid() {
		return false
	}
	filename := fset.Position(fn.Pos()).Filename
	if filename == "" {
		return false
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return false
	}
	lines := strings.SplitN(string(data), "\n", 20)
	return detectGeneratedFile(filename, lines)
}

// parsePkgFilter splits a comma-separated pkg filter string into trimmed prefixes.
func parsePkgFilter(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

