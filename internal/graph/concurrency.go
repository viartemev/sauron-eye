package graph

import (
	"regexp"
	"strings"
)

// tableExtractRegexps are regexes to find table names in SQL queries.
var tableExtractRegexps = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bFROM\s+["` + "`" + `]?(\w+)["` + "`" + `]?`),
	regexp.MustCompile(`(?i)\bINTO\s+["` + "`" + `]?(\w+)["` + "`" + `]?`),
	regexp.MustCompile(`(?i)\bUPDATE\s+["` + "`" + `]?(\w+)["` + "`" + `]?`),
	regexp.MustCompile(`(?i)\bJOIN\s+["` + "`" + `]?(\w+)["` + "`" + `]?`),
}

// SharedResourceAnalyzer finds resources accessed by multiple entry points.
type SharedResourceAnalyzer struct{}

// NewSharedResourceAnalyzer creates a new analyzer.
func NewSharedResourceAnalyzer() *SharedResourceAnalyzer {
	return &SharedResourceAnalyzer{}
}

// Analyze walks all call graphs and finds shared resources written by 2+ entry points.
func (a *SharedResourceAnalyzer) Analyze(graphs []CallGraph) []ConcurrencyConflict {
	if len(graphs) < 2 {
		return nil
	}

	// Build inverted index: resource → []entryPointIndex
	type accessInfo struct {
		epIdx    int
		hasWrite bool
	}

	// resource (table name or qualified function name) → accesses
	index := make(map[string][]accessInfo)

	for i, g := range graphs {
		resources := collectResources(g.Root)
		for res, hasWrite := range resources {
			index[res] = append(index[res], accessInfo{epIdx: i, hasWrite: hasWrite})
		}
	}

	// Find conflicts: resource touched by 2+ entry points, at least one WRITE.
	var conflicts []ConcurrencyConflict
	for resource, accesses := range index {
		if len(accesses) < 2 {
			continue
		}

		hasWrite := false
		for _, a := range accesses {
			if a.hasWrite {
				hasWrite = true
				break
			}
		}
		if !hasWrite {
			continue
		}

		// Collect unique entry points
		seen := map[int]bool{}
		var eps []EntryPoint
		for _, acc := range accesses {
			if !seen[acc.epIdx] {
				seen[acc.epIdx] = true
				eps = append(eps, graphs[acc.epIdx].EntryPoint)
			}
		}

		if len(eps) < 2 {
			continue
		}

		// Detect protection mechanisms by scanning all involved graphs
		var protection ConcurrencyProtection
		for _, acc := range accesses {
			p := detectProtection(graphs[acc.epIdx].Root, resource)
			mergeProtection(&protection, p)
		}

		conflicts = append(conflicts, ConcurrencyConflict{
			Resource:    resource,
			EntryPoints: eps,
			Protection:  protection,
		})
	}

	return conflicts
}

// collectResources walks a call node tree and returns a map of
// resource → hasWrite for all DB resources accessed.
func collectResources(node *CallNode) map[string]bool {
	if node == nil {
		return nil
	}

	result := make(map[string]bool)

	for _, dbCall := range node.DBCalls {
		tables := extractTablesFromQuery(dbCall.Query)
		isWrite := isWriteMethod(dbCall.Method)
		for _, table := range tables {
			if table == "" {
				continue
			}
			// Write-wins: once a resource is marked as written, never downgrade.
			if isWrite {
				result[table] = true
			} else if _, exists := result[table]; !exists {
				result[table] = false
			}
		}

		// Also use the qualified function name as a resource key
		if dbCall.Query == "" {
			key := dbCall.Package + "." + dbCall.Method
			isW := isWriteMethod(dbCall.Method)
			if isW {
				result[key] = true
			} else if _, exists := result[key]; !exists {
				result[key] = false
			}
		}
	}

	for _, child := range node.Children {
		if child.Callee == nil {
			continue
		}
		for k, v := range collectResources(child.Callee) {
			// Write-wins: never downgrade a write to a read.
			if v {
				result[k] = true
			} else if _, exists := result[k]; !exists {
				result[k] = false
			}
		}
	}

	return result
}

// extractTablesFromQuery extracts table names from a SQL query string.
func extractTablesFromQuery(query string) []string {
	if query == "" {
		return nil
	}
	seen := map[string]bool{}
	var tables []string
	for _, re := range tableExtractRegexps {
		matches := re.FindAllStringSubmatch(query, -1)
		for _, m := range matches {
			if len(m) >= 2 {
				t := strings.ToLower(m[1])
				if !seen[t] {
					seen[t] = true
					tables = append(tables, t)
				}
			}
		}
	}
	return tables
}

// isWriteMethod returns true if the DB method name is a write operation.
func isWriteMethod(method string) bool {
	writes := map[string]bool{
		"Exec": true, "ExecContext": true,
		"Save": true, "Create": true, "Update": true, "Updates": true, "Delete": true,
		"NamedExec": true,
	}
	return writes[method]
}

// detectProtection scans a call graph subtree for concurrency protection patterns.
func detectProtection(node *CallNode, resource string) ConcurrencyProtection {
	if node == nil {
		return ConcurrencyProtection{}
	}

	var p ConcurrencyProtection

	// Check SQL patterns in DB calls for this node
	for _, dbCall := range node.DBCalls {
		q := strings.ToLower(dbCall.Query)
		isWrite := isWriteMethod(dbCall.Method)

		if strings.Contains(q, "for update") || strings.Contains(q, "for share") {
			p.HasPessimisticLock = true
		}
		// Optimistic lock: only meaningful on write queries that include a
		// version/timestamp column in the WHERE clause (e.g. UPDATE ... WHERE version = ?).
		// A SELECT on a "versions" table or any non-write query is not an optimistic lock.
		if isWrite && (strings.Contains(q, "where version =") ||
			strings.Contains(q, "where version=") ||
			strings.Contains(q, "where updated_at =") ||
			strings.Contains(q, "where updated_at=")) {
			p.HasOptimisticLock = true
		}
		// Atomic UPDATE: UPDATE ... SET status=X WHERE status=Y (state-machine style).
		// Require both a write method AND a status/state predicate to avoid treating
		// every primary-key UPDATE as a protection mechanism.
		if isWrite && (strings.Contains(q, "where status") || strings.Contains(q, "where state")) {
			p.HasAtomicUpdate = true
		}
	}

	// Redis distributed lock
	for _, redisOp := range node.RedisOps {
		if redisOp.Method == "SetNX" || redisOp.Method == "Eval" {
			p.HasDistributedLock = true
		}
	}

	// Recurse
	for _, child := range node.Children {
		if child.Callee == nil {
			continue
		}
		cp := detectProtection(child.Callee, resource)
		mergeProtection(&p, cp)
	}

	return p
}

// mergeProtection OR-merges two ConcurrencyProtection structs.
func mergeProtection(dst *ConcurrencyProtection, src ConcurrencyProtection) {
	dst.HasOptimisticLock = dst.HasOptimisticLock || src.HasOptimisticLock
	dst.HasPessimisticLock = dst.HasPessimisticLock || src.HasPessimisticLock
	dst.HasAtomicUpdate = dst.HasAtomicUpdate || src.HasAtomicUpdate
	dst.HasDistributedLock = dst.HasDistributedLock || src.HasDistributedLock
	dst.HasUniqueConstraint = dst.HasUniqueConstraint || src.HasUniqueConstraint
	dst.HasIdempotencyKey = dst.HasIdempotencyKey || src.HasIdempotencyKey
}
