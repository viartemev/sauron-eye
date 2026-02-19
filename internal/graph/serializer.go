package graph

import (
	"encoding/json"
	"fmt"
	"io"
)

// SerializeOptions controls JSON serialization.
type SerializeOptions struct {
	// Pretty enables indented JSON output.
	Pretty bool
	// OmitSourceCode strips source code from nodes to reduce output size.
	OmitSourceCode bool
}

// Serialize writes the BuildResult as JSON to w.
func Serialize(result *BuildResult, w io.Writer, opts SerializeOptions) error {
	out := result
	if opts.OmitSourceCode {
		out = stripSourceCode(result)
	}

	var data []byte
	var err error
	if opts.Pretty {
		data, err = json.MarshalIndent(out, "", "  ")
	} else {
		data, err = json.Marshal(out)
	}
	if err != nil {
		return fmt.Errorf("json marshal: %w", err)
	}

	_, err = w.Write(data)
	return err
}

// stripSourceCode returns a shallow copy of result with all SourceCode fields emptied.
func stripSourceCode(result *BuildResult) *BuildResult {
	copy := *result
	copy.Graphs = make([]CallGraph, len(result.Graphs))
	for i, g := range result.Graphs {
		copy.Graphs[i] = CallGraph{
			EntryPoint: g.EntryPoint,
			Root:       stripNodeSource(g.Root),
		}
	}
	return &copy
}

// stripNodeSource recursively removes SourceCode from a CallNode tree.
func stripNodeSource(node *CallNode) *CallNode {
	if node == nil {
		return nil
	}
	n := *node
	n.SourceCode = ""
	if len(node.Children) > 0 {
		n.Children = make([]*CallEdge, len(node.Children))
		for i, edge := range node.Children {
			stripped := &CallEdge{Line: edge.Line}
			if edge.Callee != nil {
				stripped.Callee = stripNodeSource(edge.Callee)
			}
			n.Children[i] = stripped
		}
	}
	return &n
}

// Summary returns a human-readable summary of the BuildResult for stderr logging.
func Summary(result *BuildResult) string {
	entryPointCounts := map[string]int{}
	for _, g := range result.Graphs {
		entryPointCounts[string(g.EntryPoint.Source)]++
	}

	dbCallCount := 0
	for _, g := range result.Graphs {
		dbCallCount += countDBCalls(g.Root)
	}

	backend := "CHA+VTA"
	if !result.UsedVTA {
		backend = "CHA"
	}

	s := fmt.Sprintf(
		"entry_points=%d (http=%d kafka=%d grpc=%d cron=%d) db_calls=%d conflicts=%d backend=%s errors=%d",
		len(result.Graphs),
		entryPointCounts["http"],
		entryPointCounts["kafka"],
		entryPointCounts["grpc"],
		entryPointCounts["cron"],
		dbCallCount,
		len(result.Conflicts),
		backend,
		len(result.Errors),
	)
	return s
}

// countDBCalls recursively counts total DB calls in a subtree.
func countDBCalls(node *CallNode) int {
	if node == nil {
		return 0
	}
	n := len(node.DBCalls)
	for _, child := range node.Children {
		if child.Callee != nil {
			n += countDBCalls(child.Callee)
		}
	}
	return n
}
