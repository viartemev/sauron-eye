// Command review analyses a Go project, lets you pick an entry point,
// and sends its call graph to Claude for a structured code review.
//
// Usage:
//
//	review [flags] [project-path]
//
// Flags:
//
//	-api-key string    Anthropic API key (default: $ANTHROPIC_API_KEY)
//	-model string      Claude model (default: claude-opus-4-6)
//	-max-depth int     Maximum call graph traversal depth (default: 15)
//	-checks string     Path to checks.md file (default: <project>/checks.md)
//	-select int        Pre-select entry point number, skipping interactive prompt
//	-no-source         Omit source code from call graph sent to LLM (reduces tokens)
//	-output string     Write LLM response to file instead of stdout
//	-verbose           Print progress and token usage to stderr
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"

	_ "sauron-eye/internal/graph/entrypoints/frameworks/all"
	"strings"

	"sauron-eye/internal/graph"
	"sauron-eye/internal/review"
)

func main() {
	var (
		apiKey    = flag.String("api-key", "", "Anthropic API key (default: $ANTHROPIC_API_KEY)")
		model     = flag.String("model", "claude-opus-4-6", "Claude model to use")
		maxDepth  = flag.Int("max-depth", 15, "Maximum call graph traversal depth")
		checksArg = flag.String("checks", "", "Path to checks file (default: <project>/checks.md)")
		selectN   = flag.Int("select", 0, "Pre-select entry point by number, skipping interactive prompt")
		noSource      = flag.Bool("no-source", false, "Omit source code from call graph (reduces token usage)")
		skipGenerated = flag.Bool("skip-generated", false, "Do not recurse into auto-generated files (*.pb.go, *_gen.go)")
		outputArg = flag.String("output", "", "Write response to file instead of stdout")
		verbose   = flag.Bool("verbose", false, "Print progress and token usage to stderr")
	)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: review [flags] [project-path]\n\n")
		fmt.Fprintf(os.Stderr, "Analyse a Go project entry point and send it to Claude for review.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  review ./my-service\n")
		fmt.Fprintf(os.Stderr, "  review -select 3 -no-source ./my-service\n")
		fmt.Fprintf(os.Stderr, "  review -model claude-haiku-4-5-20251001 -output review.md .\n")
	}

	flag.Parse()

	// ── API key ───────────────────────────────────────────────────────────
	key := *apiKey
	if key == "" {
		key = os.Getenv("ANTHROPIC_API_KEY")
	}
	// key may be empty — the mock client does not require it.

	// ── Project path ──────────────────────────────────────────────────────
	projectPath := "."
	if flag.NArg() > 0 {
		projectPath = flag.Arg(0)
	}
	absProject, err := filepath.Abs(projectPath)
	if err != nil {
		fatalf("resolve project path: %v\n", err)
	}

	// ── Checks file ───────────────────────────────────────────────────────
	checksFile := *checksArg
	if checksFile == "" {
		checksFile = filepath.Join(absProject, "checks.md")
	}
	checksData, err := os.ReadFile(checksFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot read checks file %q: %v\n", checksFile, err)
		fmt.Fprintf(os.Stderr, "         continuing without project-specific checks\n")
		checksData = []byte("(no checks.md found — apply general Go best practices)")
	}

	// ── Suppress internal builder logs unless -verbose ────────────────────
	if !*verbose {
		log.SetOutput(io.Discard)
	}

	// ── Build call graph ──────────────────────────────────────────────────
	progressf("Building call graph for %s …\n", absProject)
	progressf("(this typically takes 30–90 seconds)\n")

	cfg := graph.Config{
		ProjectPath:   absProject,
		MaxDepth:      *maxDepth,
		SkipGenerated: *skipGenerated,
	}
	builder := graph.NewCallGraphBuilder(cfg)
	result, err := builder.Build(context.Background())
	if err != nil {
		fatalf("build call graph: %v\n", err)
	}

	if len(result.Errors) > 0 && *verbose {
		fmt.Fprintln(os.Stderr, "build warnings:")
		for _, e := range result.Errors {
			fmt.Fprintf(os.Stderr, "  • %s\n", e)
		}
	}

	if len(result.Graphs) == 0 {
		fatalf("no entry points found — is this a Go service? (HTTP/Kafka/gRPC/cron handlers required)\n")
	}

	// ── Display entry point list ──────────────────────────────────────────
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "Module: %s\n", result.Module)
	fmt.Fprintf(os.Stderr, "Found %d entry point(s):\n\n", len(result.Graphs))

	maxLabel := 0
	labels := make([]string, len(result.Graphs))
	for i, g := range result.Graphs {
		labels[i] = formatEntryPoint(g.EntryPoint)
		if len(labels[i]) > maxLabel {
			maxLabel = len(labels[i])
		}
	}

	for i, lbl := range labels {
		fmt.Fprintf(os.Stderr, "  [%2d]  %s\n", i+1, lbl)
	}
	fmt.Fprintln(os.Stderr)

	// ── Select entry point ────────────────────────────────────────────────
	chosen := *selectN
	if chosen == 0 {
		fmt.Fprintf(os.Stderr, "Enter number (1–%d): ", len(result.Graphs))
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			os.Exit(0)
		}
		n, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		if err != nil || n < 1 || n > len(result.Graphs) {
			fatalf("invalid selection\n")
		}
		chosen = n
	}

	if chosen < 1 || chosen > len(result.Graphs) {
		fatalf("-select %d is out of range (1–%d)\n", chosen, len(result.Graphs))
	}

	selectedGraph := result.Graphs[chosen-1]
	progressf("\nSelected: %s\n", formatEntryPoint(selectedGraph.EntryPoint))

	// ── Build prompt ──────────────────────────────────────────────────────
	input := review.PromptInput{
		Module:     result.Module,
		Graph:      selectedGraph,
		Conflicts:  result.Conflicts,
		Checks:     string(checksData),
		OmitSource: *noSource,
	}

	systemPrompt := review.SystemPrompt()
	userMessage := review.BuildUserMessage(input)

	if *verbose {
		fmt.Fprintf(os.Stderr, "prompt size: system=%d chars, user=%d chars\n",
			len(systemPrompt), len(userMessage))
	}

	// ── Call Claude ───────────────────────────────────────────────────────
	progressf("Sending to Claude (%s) …\n", *model)

	response, inputTokens, outputTokens, err := review.Analyze(context.Background(), review.AnalyzeRequest{
		APIKey:  key,
		Model:   *model,
		System:  systemPrompt,
		Message: userMessage,
	})
	if err != nil {
		fatalf("Claude API: %v\n", err)
	}

	if *verbose {
		fmt.Fprintf(os.Stderr, "tokens used: input=%d output=%d\n", inputTokens, outputTokens)
	}

	// ── Output ────────────────────────────────────────────────────────────
	out := os.Stdout
	if *outputArg != "" {
		f, err := os.Create(*outputArg)
		if err != nil {
			fatalf("create output file: %v\n", err)
		}
		defer f.Close()
		out = f
		progressf("Writing response to %s\n", *outputArg)
	}

	fmt.Fprintln(out, response)
}

// formatEntryPoint returns a human-readable one-liner for an entry point.
func formatEntryPoint(ep graph.EntryPoint) string {
	switch ep.Source {
	case graph.SourceHTTP:
		method := ep.Method
		if method == "" {
			method = "ANY"
		}
		path := ep.Path
		if path == "" {
			path = "(unknown path)"
		}
		return fmt.Sprintf("HTTP %-7s %-35s → %s", method, path, ep.FunctionName)
	case graph.SourceKafka:
		topic := ep.Topic
		if topic == "" {
			topic = "(unknown topic)"
		}
		return fmt.Sprintf("Kafka   topic=%-30s → %s", topic, ep.FunctionName)
	case graph.SourceGRPC:
		path := ep.Path
		if path == "" {
			path = "(unknown method)"
		}
		return fmt.Sprintf("gRPC    %-35s → %s", path, ep.FunctionName)
	case graph.SourceCron:
		name := ep.Path
		if name == "" {
			name = "(unnamed)"
		}
		return fmt.Sprintf("Cron    %-35s → %s", name, ep.FunctionName)
	default:
		return fmt.Sprintf("%-40s → %s", string(ep.Source), ep.FunctionName)
	}
}

func progressf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format, args...)
	os.Exit(1)
}
