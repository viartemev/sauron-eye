package graph

import (
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"
)

// generatedNameSuffixes lists filename suffixes that mark auto-generated Go files.
var generatedNameSuffixes = []string{
	".pb.go",          // protobuf
	"_gen.go",         // common codegen convention
	".gen.go",         // alternative convention
	"_generated.go",   // another common suffix
	".generated.go",   // alternative
	"_mock.go",        // mockery / counterfeiter mocks
	"_easyjson.go",    // easyjson
	"_ffjson.go",      // ffjson
	"_enumer.go",      // enumer
	".deepcopy.go",    // controller-gen deepcopy
}

// visitedSet tracks which functions have been visited in the current DFS path.
// Using a "sliding window": we add on entry and remove on return so sibling
// branches can still visit the same function.
type visitedSet map[*ssa.Function]struct{}

// Traversal builds CallNode trees from a callgraph.
type Traversal struct {
	maxDepth      int
	prog          *ssa.Program
	cg            *callgraph.Graph
	fset          *token.FileSet
	goroot        string // GOROOT prefix — skip recursion into stdlib source
	modCache      string // module cache prefix — skip recursion into dep source
	fileCache     map[string][]string
	genCache      map[string]bool // cache: filename → is generated
	consumers     ChannelConsumerIndex
	pkgPrefixes   []string // if non-empty, only recurse into matching packages
	skipGenerated bool     // when true, don't recurse into generated source files
}

// NewTraversal creates a new Traversal.
func NewTraversal(prog *ssa.Program, cg *callgraph.Graph, fset *token.FileSet, maxDepth int, consumers ChannelConsumerIndex, pkgPrefixes []string, skipGenerated bool) *Traversal {
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = filepath.Join(os.Getenv("HOME"), "go")
	}
	return &Traversal{
		maxDepth:      maxDepth,
		prog:          prog,
		cg:            cg,
		fset:          fset,
		goroot:        runtime.GOROOT(),
		modCache:      filepath.Join(gopath, "pkg", "mod"),
		fileCache:     make(map[string][]string),
		genCache:      make(map[string]bool),
		consumers:     consumers,
		pkgPrefixes:   pkgPrefixes,
		skipGenerated: skipGenerated,
	}
}

// BuildNode recursively builds a CallNode tree rooted at fn.
func (t *Traversal) BuildNode(fn *ssa.Function, depth int, visited visitedSet, hasTxContext bool) *CallNode {
	if depth > t.maxDepth {
		return &CallNode{
			FunctionName: fn.RelString(nil),
			Truncated:    true,
			TruncReason:  "depth",
		}
	}
	if _, ok := visited[fn]; ok {
		return &CallNode{
			FunctionName: fn.RelString(nil),
			Truncated:    true,
			TruncReason:  "cycle",
		}
	}

	// Sliding window: add now, remove when this stack frame returns.
	visited[fn] = struct{}{}
	defer delete(visited, fn)

	// Build the node.
	node := &CallNode{
		FunctionName: fn.RelString(nil),
		Category:     categorizeFunction(fn),
	}

	// Position info.
	if fn.Pos().IsValid() {
		pos := t.fset.Position(fn.Pos())
		node.File = pos.Filename
		node.Line = pos.Line
	}

	// Mark generated source files.
	if node.File != "" {
		node.Generated = t.isGeneratedFile(node.File)
	}

	// Package name.
	if fn.Package() != nil {
		node.Package = fn.Package().Pkg.Path()
	}

	// Extract metadata from SSA instructions.
	meta := ExtractMetadata(fn, t.fset, hasTxContext)
	node.DBCalls = meta.DBCalls
	node.TxOps = meta.TxOps
	node.HTTPCalls = meta.HTTPCalls
	node.KafkaOps = meta.KafkaOps
	node.SyncOps = meta.SyncOps
	node.RedisOps = meta.RedisOps
	node.ChannelOps = meta.ChannelOps

	// Extract source code (only for project files, not module cache / GOROOT).
	if !t.isExternal(fn) {
		node.SourceCode = t.extractSource(fn)
	}

	// Determine if a transaction is open in children.
	childTxContext := hasTxContext
	for _, op := range meta.TxOps {
		if op.Operation == "Begin" || op.Operation == "BeginTx" || op.Operation == "Transaction" {
			childTxContext = true
		}
	}

	// Recurse into call graph edges — but only into project code, not into
	// stdlib / module-cache code (those are recorded in metadata, not traversed).
	//
	// addedCallees tracks which *ssa.Function have already been added so the
	// supplementary SSA scan below can avoid adding duplicates.
	addedCallees := make(map[*ssa.Function]bool)

	cgNode := t.cg.Nodes[fn]
	if cgNode != nil {
		for _, edge := range cgNode.Out {
			callee := edge.Callee.Func
			if callee == nil || t.shouldSkip(callee) {
				continue
			}

			// Drop dynamic call edges (function-value calls) from generated source
			// files. CHA over-approximates these — e.g. oapi-codegen's applyEditors
			// calls RequestEditorFn through a func variable and CHA resolves it to
			// every project function with a matching signature, producing false edges.
			// Static calls from generated code are still captured by the
			// supplementary SSA scan below, so nothing is lost.
			if node.Generated && edge.Site != nil && edge.Site.Common().StaticCallee() == nil {
				continue
			}

			edgeLine := 0
			isDeferred := false
			if edge.Site != nil {
				if edge.Site.Pos().IsValid() {
					edgeLine = t.fset.Position(edge.Site.Pos()).Line
				}
				_, isDeferred = edge.Site.(*ssa.Defer)
			}

			addedCallees[callee] = true
			childNode := t.BuildNode(callee, depth+1, visited, childTxContext)
			node.Children = append(node.Children, &CallEdge{
				Callee:   childNode,
				Line:     edgeLine,
				Deferred: isDeferred,
			})
		}
	}

	// Supplementary SSA scan: walk the function's own instructions and add
	// any static callees that CHA/VTA did not record in the call graph.
	// This catches direct method calls on concrete types that the call graph
	// analysis occasionally misses (e.g. calls into oapi-codegen generated
	// clients where the receiver struct embeds an interface).
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			callInstr, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}
			callee := callInstr.Common().StaticCallee()
			if callee == nil || addedCallees[callee] || t.shouldSkip(callee) {
				continue
			}
			addedCallees[callee] = true

			edgeLine := 0
			if callInstr.Pos().IsValid() {
				edgeLine = t.fset.Position(callInstr.Pos()).Line
			}
			_, isDeferred := callInstr.(*ssa.Defer)

			childNode := t.BuildNode(callee, depth+1, visited, childTxContext)
			node.Children = append(node.Children, &CallEdge{
				Callee:   childNode,
				Line:     edgeLine,
				Deferred: isDeferred,
			})
		}
	}

	// Synthetic channel edges: for each channel send in this node, find all
	// project functions that receive from a channel of the same element type
	// and add them as children marked Via:"channel".
	for _, op := range meta.ChannelOps {
		if op.Direction != "send" {
			continue
		}
		for _, consumer := range t.consumers[op.ChanType] {
			if t.shouldSkip(consumer) {
				continue
			}
			childNode := t.BuildNode(consumer, depth+1, visited, false)
			node.Children = append(node.Children, &CallEdge{
				Callee: childNode,
				Line:   op.Line,
				Via:    "channel",
			})
		}
	}

	return node
}

// shouldSkip returns true for functions we should not recurse into:
// synthetic wrappers, stdlib, external module dependencies, and packages
// outside the configured pkg filter.
func (t *Traversal) shouldSkip(fn *ssa.Function) bool {
	if fn == nil || fn.Package() == nil {
		return true
	}
	// Synthetic functions (interface method wrappers, bound-method thunks) —
	// skip to avoid explosion; they don't contain real logic.
	if fn.Synthetic != "" {
		return true
	}
	if t.isExternal(fn) {
		return true
	}
	// Skip generated files when configured.
	if t.skipGenerated && fn.Pos().IsValid() {
		if t.isGeneratedFile(t.fset.Position(fn.Pos()).Filename) {
			return true
		}
	}
	// Package filter: if prefixes are configured, only recurse into matching packages.
	if len(t.pkgPrefixes) > 0 {
		pkg := fn.Package().Pkg.Path()
		for _, prefix := range t.pkgPrefixes {
			if strings.HasPrefix(pkg, prefix) {
				return false
			}
		}
		return true
	}
	return false
}

// isExternal returns true if fn's source file lives outside the project —
// i.e. in GOROOT (stdlib) or the module cache (third-party deps).
func (t *Traversal) isExternal(fn *ssa.Function) bool {
	if !fn.Pos().IsValid() {
		return true // no position → synthetic / no source
	}
	file := t.fset.Position(fn.Pos()).Filename
	if file == "" {
		return true
	}
	// Clean the path so prefix comparisons are reliable.
	file = filepath.Clean(file)
	return strings.HasPrefix(file, t.goroot) ||
		strings.HasPrefix(file, t.modCache)
}

// categorizeFunction assigns a NodeCategory based on package path heuristics.
func categorizeFunction(fn *ssa.Function) NodeCategory {
	if fn.Package() == nil {
		return CategoryUnknown
	}
	path := strings.ToLower(fn.Package().Pkg.Path())

	if strings.Contains(path, "net/http") {
		return CategoryHTTPClient
	}
	if strings.Contains(path, "kafka") || strings.Contains(path, "producer") {
		return CategoryProducer
	}
	for _, kw := range []string{"repo", "repository", "store", "storage", "dao", "persistence"} {
		if strings.Contains(path, kw) {
			return CategoryRepository
		}
	}
	for _, kw := range []string{"service", "svc", "usecase", "domain"} {
		if strings.Contains(path, kw) {
			return CategoryService
		}
	}
	for _, kw := range []string{"handler", "api", "grpc", "controller", "delivery"} {
		if strings.Contains(path, kw) {
			return CategoryHandler
		}
	}
	return CategoryUnknown
}

// extractSource extracts the source code of a function (up to 50 lines).
func (t *Traversal) extractSource(fn *ssa.Function) string {
	if fn.Syntax() == nil || !fn.Pos().IsValid() {
		return ""
	}

	startPos := t.fset.Position(fn.Pos())
	endPos := t.fset.Position(fn.Syntax().End())

	if startPos.Filename == "" {
		return ""
	}

	lines := t.readFileLines(startPos.Filename)
	if lines == nil {
		return ""
	}

	startLine := startPos.Line - 1 // convert to 0-indexed
	endLine := endPos.Line          // inclusive end, 0-indexed = endPos.Line-1, +1 to include

	if startLine < 0 {
		startLine = 0
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	if endLine-startLine > 150 {
		endLine = startLine + 150
	}

	return strings.Join(lines[startLine:endLine], "\n")
}

// isGeneratedFile returns true if the source file was auto-generated.
// Results are cached to avoid repeated I/O and regex work.
func (t *Traversal) isGeneratedFile(filename string) bool {
	if filename == "" {
		return false
	}
	if cached, ok := t.genCache[filename]; ok {
		return cached
	}
	result := detectGeneratedFile(filename, t.readFileLines(filename))
	t.genCache[filename] = result
	return result
}

// detectGeneratedFile checks a filename and its first few lines for generated-file markers.
func detectGeneratedFile(filename string, lines []string) bool {
	// Name-based check (fast path).
	base := filepath.Base(filename)
	for _, suf := range generatedNameSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	// mock_ prefix (e.g. mock_store.go).
	if strings.HasPrefix(base, "mock_") && strings.HasSuffix(base, ".go") {
		return true
	}

	// Content-based: standard "// Code generated" marker (Go spec).
	// Only scan the first 10 lines to keep it fast.
	for i, line := range lines {
		if i >= 10 {
			break
		}
		if strings.Contains(line, "Code generated") {
			return true
		}
	}
	return false
}

// readFileLines reads and caches the lines of a file.
func (t *Traversal) readFileLines(filename string) []string {
	if cached, ok := t.fileCache[filename]; ok {
		return cached
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.fileCache[filename] = nil
		return nil
	}
	lines := strings.Split(string(data), "\n")
	t.fileCache[filename] = lines
	return lines
}
