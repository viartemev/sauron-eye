// Command callgraph analyses a Go project and outputs its call graphs as JSON.
//
// Usage:
//
//	callgraph [flags] <project-path>
//
// Flags:
//
//	-output string       Output file path (default: stdout)
//	-max-depth int       Maximum call graph depth (default: 6)
//	-pta-timeout int     Pointer analysis timeout in seconds (default: 180)
//	-scope string        Comma-separated package patterns (default: ./...)
//	-format string       Output format: json or pretty (default: pretty)
//	-no-source           Omit source code from output
//	-verbose             Print progress to stderr
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"sauron-eye/internal/graph"
)

func main() {
	var (
		outputFile = flag.String("output", "", "Output file path (default: stdout)")
		maxDepth   = flag.Int("max-depth", 15, "Maximum call graph traversal depth")
		_          = flag.Int("pta-timeout", 180, "Deprecated: no longer used (analysis is now CHA+VTA)")
		scope      = flag.String("scope", "./...", "Comma-separated package patterns to analyse")
		pkgFilter  = flag.String("pkg-filter", "", "Comma-separated package path prefixes to include (default: module name from go.mod)")
		format     = flag.String("format", "pretty", "Output format: json or pretty")
		noSource      = flag.Bool("no-source", false, "Omit source code from output to reduce size")
		skipGenerated = flag.Bool("skip-generated", false, "Do not recurse into auto-generated source files (*.pb.go, *_gen.go, files with \"Code generated\" header, etc.)")
		verbose       = flag.Bool("verbose", false, "Print progress information to stderr")
	)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: callgraph [flags] <project-path>\n\n")
		fmt.Fprintf(os.Stderr, "Analyse a Go project and output call graphs as JSON.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  callgraph ./my-service\n")
		fmt.Fprintf(os.Stderr, "  callgraph -format pretty -max-depth 4 /path/to/project\n")
		fmt.Fprintf(os.Stderr, "  callgraph -no-source -output graphs.json .\n")
		fmt.Fprintf(os.Stderr, "  callgraph -pkg-filter mymodule/internal ./my-service\n")
	}

	flag.Parse()

	projectPath := "."
	if flag.NArg() > 0 {
		projectPath = flag.Arg(0)
	}

	// Resolve project path to absolute.
	if projectPath == "" {
		projectPath = "."
	}

	if !*verbose {
		// Suppress internal log output unless -verbose is set.
		log.SetOutput(io.Discard)
	}

	// Build config.
	patterns := strings.Split(*scope, ",")
	for i, p := range patterns {
		patterns[i] = strings.TrimSpace(p)
	}

	cfg := graph.Config{
		ProjectPath:   projectPath,
		MaxDepth:      *maxDepth,
		ScopePatterns: patterns,
		PkgFilter:     *pkgFilter,
		SkipGenerated: *skipGenerated,
	}

	if *verbose {
		fmt.Fprintf(os.Stderr, "[callgraph] analysing %s\n", projectPath)
		fmt.Fprintf(os.Stderr, "[callgraph] max-depth=%d scope=%s\n", *maxDepth, *scope)
	}

	// Run the analysis.
	ctx := context.Background()
	builder := graph.NewCallGraphBuilder(cfg)

	start := time.Now()
	result, err := builder.Build(ctx)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Print summary to stderr.
	fmt.Fprintf(os.Stderr, "[callgraph] done in %s — %s\n", elapsed.Round(time.Millisecond), graph.Summary(result))

	if len(result.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "[callgraph] warnings:\n")
		for _, e := range result.Errors {
			fmt.Fprintf(os.Stderr, "  - %s\n", e)
		}
	}

	// Determine output writer.
	out := os.Stdout
	if *outputFile != "" {
		f, err := os.Create(*outputFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error creating output file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
	}

	// Serialize result.
	opts := graph.SerializeOptions{
		Pretty:         *format == "pretty",
		OmitSourceCode: *noSource,
	}

	if err := graph.Serialize(result, out, opts); err != nil {
		fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
		os.Exit(1)
	}

	// Newline after JSON when writing to stdout.
	if *outputFile == "" {
		fmt.Fprintln(out)
	}
}
