package analyzer

import (
	"fmt"
	"sort"

	"pad-core/models"
)

// relatedRules is an EDITORIAL map: a hand-maintained list of rule pairs whose
// findings are worth reviewing together, each with a one-line reason. It is
// what GET /api/analysis/dependencies serves and what the "Related rules" view
// draws.
//
// Nothing in the engine consumes it. Rules are independent — every enabled
// rule is dispatched per block regardless of any other rule's result — so the
// order returned alongside it (TopoOrder) is a presentation order for the map,
// not an execution constraint, and a rule absent from the map is merely
// unrelated in the author's judgement, not "isolated" by the analyzer. It used
// to be presented as a dependency graph, which implied an ordering the
// analyzer never had; keep it honest by adding a pair only when one rule's
// finding is a genuine reason to look at the other.
var relatedRules = []models.RuleDependency{
	{FromRuleID: "dead-code", ToRuleID: "unused-variable", Reason: "dead code may produce unused variable findings"},
	{FromRuleID: "deep-nesting", ToRuleID: "infinite-loop-risk", Reason: "deep nesting increases loop complexity"},
	{FromRuleID: "unhandled-error", ToRuleID: "error-swallow", Reason: "error handling must be checked before swallow detection"},
	{FromRuleID: "file-op-no-error-handler", ToRuleID: "unhandled-error", Reason: "file operations are a subset of error handling"},
	{FromRuleID: "missing-retry", ToRuleID: "missing-timeout", Reason: "retry and timeout checks are related reliability concerns"},
	{FromRuleID: "subflow-no-error-handler", ToRuleID: "unhandled-error", Reason: "subflow error handling relates to general error handling"},
	{FromRuleID: "empty-branch", ToRuleID: "dead-code", Reason: "empty branches may indicate dead code paths"},
	{FromRuleID: "goto-antipattern", ToRuleID: "infinite-loop-risk", Reason: "goto can create hidden loop structures"},
	{FromRuleID: "sensitive-exposure", ToRuleID: "hardcoded-credential", Reason: "sensitive exposure may involve hardcoded credentials"},
}

// RelatedRulesDescription travels with the map so every consumer (API, UI,
// CLI) can tell the user what it is and is not.
const RelatedRulesDescription = "Hand-maintained editorial map of rules whose findings are worth reviewing together. " +
	"It is not derived from the analyzer: rules run independently, and this map affects neither evaluation order nor results."

// AnalyzeRuleDependencies returns the related-rules map restricted to rules
// present in the catalog, plus a cycle check on the map and a topological
// presentation order over all rules. The name and the response shape are kept
// for the existing route; the payload is marked Editorial so consumers do not
// read it as an execution dependency graph.
func AnalyzeRuleDependencies() *models.DependencyAnalysis {
	rules := AllRules()
	ruleIDs := make(map[string]bool)
	for _, r := range rules {
		ruleIDs[r.ID()] = true
	}

	var active []models.RuleDependency
	for _, dep := range relatedRules {
		if ruleIDs[dep.FromRuleID] && ruleIDs[dep.ToRuleID] {
			active = append(active, dep)
		}
	}

	adj := make(map[string][]string)
	for _, dep := range active {
		adj[dep.FromRuleID] = append(adj[dep.FromRuleID], dep.ToRuleID)
	}

	cycles := detectCycles(adj, ruleIDs)

	order := topoSort(rules, adj)

	return &models.DependencyAnalysis{
		Dependencies: active,
		Cycles:       cycles,
		TopoOrder:    order,
		Editorial:    true,
		Description:  RelatedRulesDescription,
	}
}

// detectCycles returns the cyclic strongly-connected components of the
// related-rules map. Each cycle is the set of rules forming an SCC, sorted for
// determinism (rotations are equivalent, so a canonical order avoids the
// "which node leads?" ambiguity the prior DFS-with-colors reconstruction had —
// it emitted a duplicate leading element in some rotations).
//
// An SCC is reported as a cycle iff it has more than one node, or a single node
// with a self-loop. Tarjan's algorithm finds SCCs in O(V+E); the rule graph is
// tiny (45 nodes — see registry_pin_test.go) so the textbook recursive form is safe here (unbounded
// user-input graphs elsewhere get recursion guards — this graph is fixed/
// internal).
func detectCycles(adj map[string][]string, nodes map[string]bool) [][]string {
	index := make(map[string]int)
	lowlink := make(map[string]int)
	onStack := make(map[string]bool)
	var stack []string
	idx := 0
	var sccs [][]string

	var strongconnect func(v string)
	strongconnect = func(v string) {
		index[v] = idx
		lowlink[v] = idx
		idx++
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range adj[v] {
			if _, seen := index[w]; !seen {
				strongconnect(w)
				if lowlink[w] < lowlink[v] {
					lowlink[v] = lowlink[w]
				}
			} else if onStack[w] {
				if index[w] < lowlink[v] {
					lowlink[v] = index[w]
				}
			}
		}

		// v roots an SCC when its lowlink equals its index.
		if lowlink[v] == index[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			sccs = append(sccs, scc)
		}
	}

	// Visit in a deterministic order so the emitted cycle list is stable.
	nodeList := make([]string, 0, len(nodes))
	for n := range nodes {
		nodeList = append(nodeList, n)
	}
	sort.Strings(nodeList)
	for _, v := range nodeList {
		if _, seen := index[v]; !seen {
			strongconnect(v)
		}
	}

	var cycles [][]string
	for _, scc := range sccs {
		switch {
		case len(scc) > 1:
			sort.Strings(scc) // canonical order — no duplicate/leading-element quirks
			cycles = append(cycles, scc)
		case len(scc) == 1:
			// A singleton is only cyclic if it has a self-loop.
			v := scc[0]
			for _, w := range adj[v] {
				if w == v {
					cycles = append(cycles, scc)
					break
				}
			}
		}
	}
	if cycles == nil {
		cycles = [][]string{}
	}
	return cycles
}

func topoSort(rules []Rule, adj map[string][]string) []string {
	inDegree := make(map[string]int)
	for _, r := range rules {
		inDegree[r.ID()] = 0
	}
	for _, neighbors := range adj {
		for _, n := range neighbors {
			inDegree[n]++
		}
	}

	var queue []string
	for _, r := range rules {
		if inDegree[r.ID()] == 0 {
			queue = append(queue, r.ID())
		}
	}

	var order []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		order = append(order, node)
		for _, next := range adj[node] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if order == nil {
		order = []string{}
	}
	return order
}

func ComputeSubflowHashes(doc *models.FlowDocument) []models.SubflowHash {
	var hashes []models.SubflowHash
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		h := computeSubflowHash(sf)
		hashes = append(hashes, models.SubflowHash{
			SubflowID: sf.ID,
			Hash:      h,
		})
	}
	return hashes
}

func computeSubflowHash(sf *models.Subflow) string {
	d := fnvBuilder()
	d.write(sf.Name)
	walkSubflowBlocks(sf, func(b *models.Block) {
		// b.ID/sf.ID excluded — parser-minted UUIDs, unstable across re-parses.
		d.write(b.Name)
		d.write(string(b.Type))
		d.write(b.RawType)
		d.write(fmt.Sprintf("%d", b.Indent))
		// Sort keys for a deterministic hash — Go randomizes map iteration, so
		// unsorted hashing makes identical subflows hash differently and breaks
		// subflow dedup/comparison.
		keys := make([]string, 0, len(b.Properties))
		for k := range b.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			d.write(k)
			d.write(b.Properties[k])
		}
	})
	return d.sum()
}

type fnvHasher struct {
	h uint32
}

func fnvBuilder() *fnvHasher {
	return &fnvHasher{h: 2166136261}
}

func (f *fnvHasher) write(s string) {
	// FNV-1a operates on the bytes of the string, not its runes. Iterating
	// runes conflates distinct UTF-8 byte sequences that map to the same code
	// point and produces non-canonical hashes for any non-ASCII content.
	for i := 0; i < len(s); i++ {
		f.h ^= uint32(s[i])
		f.h *= 16777619
	}
}

func (f *fnvHasher) sum() string {
	return fmt.Sprintf("%08x", f.h)
}

// RunIncrementalAnalysis (subflow-scoped partial re-analysis) was removed as
// dead code: it was never wired into any caller, and the cache in cache.go
// (content hash + settings digest) already makes full re-analysis cheap.
