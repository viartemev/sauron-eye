package review

import (
	"encoding/json"
	"fmt"
	"strings"

	"sauron-eye/internal/graph"
)

// PromptInput is everything needed to build the review prompt.
type PromptInput struct {
	Module    string
	Graph     graph.CallGraph
	Conflicts []graph.ConcurrencyConflict
	Checks    string
	// OmitSource strips SourceCode from nodes before serialising.
	// Useful for large call trees that would exceed token limits.
	OmitSource bool
}

// SystemPrompt returns the static system instruction for Claude.
func SystemPrompt() string {
	return `You are an expert Go backend engineer specialising in production reliability and security.
Your job is to find real bugs and architectural problems in Go code by analysing call graphs.
You report only actual problems — not style issues, not theoretical concerns without evidence in the graph.
For every finding you must cite the exact file and line number from the graph.
Respond in well-structured Markdown.`
}

// BuildUserMessage constructs the user-turn message from the input.
func BuildUserMessage(input PromptInput) string {
	var b strings.Builder

	// ── Project info ──────────────────────────────────────────────────────
	b.WriteString("## Project\n\n")
	fmt.Fprintf(&b, "**Module:** `%s`\n\n", input.Module)

	// ── Entry point being reviewed ────────────────────────────────────────
	ep := input.Graph.EntryPoint
	b.WriteString("## Entry Point Under Review\n\n")
	fmt.Fprintf(&b, "| Field | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Type | `%s` |\n", ep.Source)
	if ep.Method != "" {
		fmt.Fprintf(&b, "| Method | `%s` |\n", ep.Method)
	}
	if ep.Path != "" {
		fmt.Fprintf(&b, "| Path | `%s` |\n", ep.Path)
	}
	if ep.Topic != "" {
		fmt.Fprintf(&b, "| Kafka Topic | `%s` |\n", ep.Topic)
		fmt.Fprintf(&b, "| Consumer Group | `%s` |\n", ep.ConsumerGroup)
		fmt.Fprintf(&b, "| Commit Mode | `%s` |\n", ep.CommitMode)
	}
	fmt.Fprintf(&b, "| Handler | `%s` |\n", ep.FunctionName)
	fmt.Fprintf(&b, "| File | `%s:%d` |\n", ep.File, ep.Line)
	b.WriteString("\n")

	// ── Concurrency conflicts ─────────────────────────────────────────────
	relevant := relevantConflicts(ep, input.Conflicts)
	if len(relevant) > 0 {
		b.WriteString("## Shared Resources (other entry points accessing the same data)\n\n")
		for _, c := range relevant {
			fmt.Fprintf(&b, "**Resource:** `%s`\n", c.Resource)
			b.WriteString("Also accessed by:\n")
			for _, other := range c.EntryPoints {
				if other.FunctionName == ep.FunctionName {
					continue
				}
				label := string(other.Source)
				if other.Path != "" {
					label += " " + other.Path
				} else if other.Topic != "" {
					label += " topic=" + other.Topic
				}
				fmt.Fprintf(&b, "- `%s` → `%s`\n", label, other.FunctionName)
			}
			p := c.Protection
			if !p.HasOptimisticLock && !p.HasPessimisticLock && !p.HasAtomicUpdate && !p.HasDistributedLock && !p.HasUniqueConstraint && !p.HasIdempotencyKey {
				b.WriteString("- ⚠️ **No concurrency protection detected**\n")
			} else {
				b.WriteString("- Protection: ")
				var mechanisms []string
				if p.HasPessimisticLock {
					mechanisms = append(mechanisms, "pessimistic lock (FOR UPDATE)")
				}
				if p.HasOptimisticLock {
					mechanisms = append(mechanisms, "optimistic lock (version/updated_at)")
				}
				if p.HasAtomicUpdate {
					mechanisms = append(mechanisms, "atomic conditional UPDATE")
				}
				if p.HasDistributedLock {
					mechanisms = append(mechanisms, "distributed lock (Redis)")
				}
				if p.HasUniqueConstraint {
					mechanisms = append(mechanisms, "unique constraint")
				}
				if p.HasIdempotencyKey {
					mechanisms = append(mechanisms, "idempotency key")
				}
				b.WriteString(strings.Join(mechanisms, ", "))
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}

	// ── Checks ────────────────────────────────────────────────────────────
	b.WriteString("## Checks to Apply\n\n")
	b.WriteString(input.Checks)
	b.WriteString("\n\n")

	// ── Call graph JSON ───────────────────────────────────────────────────
	b.WriteString("## Call Graph\n\n")
	b.WriteString("The JSON below represents the full call tree for this entry point.\n")
	b.WriteString("Each node includes: function name, file, line, category, source code, ")
	b.WriteString("and extracted metadata (db_calls, tx_ops, http_calls, kafka_ops, sync_ops, redis_ops).\n\n")
	b.WriteString("```json\n")
	g := input.Graph
	if input.OmitSource {
		g.Root = stripSource(g.Root)
	}
	enc, _ := json.MarshalIndent(g, "", "  ")
	b.Write(enc)
	b.WriteString("\n```\n\n")

	// ── Task ──────────────────────────────────────────────────────────────
	b.WriteString("## Your Task\n\n")
	b.WriteString("Analyse the call graph above against every check listed above.\n\n")
	b.WriteString("For each real problem found, write a section:\n\n")
	b.WriteString("```\n### [SEVERITY] Title\n\n")
	b.WriteString("**Category:** <check category>\n")
	b.WriteString("**Location:** `file.go:line` in `FunctionName()`\n\n")
	b.WriteString("<What can go wrong — concrete failure scenario>\n\n")
	b.WriteString("**Fix:** <what exactly needs to change>\n\n")
	b.WriteString("**Call path:** EntryPoint → ... → ProblematicFunction\n")
	b.WriteString("```\n\n")
	b.WriteString("Severity levels: `CRITICAL` / `HIGH` / `MEDIUM` / `LOW`\n\n")
	b.WriteString("If you find no real problems, say so explicitly. Do not invent findings.\n")

	return b.String()
}

// relevantConflicts returns conflicts where the given entry point is one of the participants.
func relevantConflicts(ep graph.EntryPoint, conflicts []graph.ConcurrencyConflict) []graph.ConcurrencyConflict {
	var out []graph.ConcurrencyConflict
	for _, c := range conflicts {
		for _, cep := range c.EntryPoints {
			if cep.FunctionName == ep.FunctionName {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// stripSource returns a deep copy of the node tree with SourceCode removed.
func stripSource(node *graph.CallNode) *graph.CallNode {
	if node == nil {
		return nil
	}
	n := *node
	n.SourceCode = ""
	if len(node.Children) > 0 {
		n.Children = make([]*graph.CallEdge, len(node.Children))
		for i, edge := range node.Children {
			e := &graph.CallEdge{Line: edge.Line, Via: edge.Via, Deferred: edge.Deferred}
			if edge.Callee != nil {
				e.Callee = stripSource(edge.Callee)
			}
			n.Children[i] = e
		}
	}
	return &n
}
