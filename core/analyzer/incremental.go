package analyzer

import (
	"fmt"
	"sort"

	"pad-core/models"
)

var knownDependencies = []models.RuleDependency{
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

func AnalyzeRuleDependencies() *models.DependencyAnalysis {
	rules := AllRules()
	ruleIDs := make(map[string]bool)
	for _, r := range rules {
		ruleIDs[r.ID()] = true
	}

	var active []models.RuleDependency
	for _, dep := range knownDependencies {
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
	}
}

func detectCycles(adj map[string][]string, nodes map[string]bool) [][]string {
	const (
		white = 0
		gray  = 1
		black = 2
	)

	color := make(map[string]int)
	parent := make(map[string]string)
	var cycles [][]string

	var dfs func(node string)
	dfs = func(node string) {
		color[node] = gray
		for _, next := range adj[node] {
			switch color[next] {
			case gray:
				cycle := []string{next}
				cur := node
				for cur != next && cur != "" {
					cycle = append([]string{cur}, cycle...)
					cur = parent[cur]
				}
				if cur == next {
					cycle = append([]string{next}, cycle...)
				}
				cycles = append(cycles, cycle)
			case white:
				parent[next] = node
				dfs(next)
			}
		}
		color[node] = black
	}

	for node := range nodes {
		if color[node] == white {
			dfs(node)
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
	d.write(sf.ID)
	d.write(sf.Name)
	walkSubflowBlocks(sf, func(b *models.Block) {
		d.write(b.ID)
		d.write(b.Name)
		d.write(string(b.Type))
		d.write(b.RawType)
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
	for _, c := range s {
		f.h ^= uint32(c) // #nosec G115 -- rune always fits in uint32; FNV-1a hashing

		f.h *= 16777619
	}
}

func (f *fnvHasher) sum() string {
	return fmt.Sprintf("%08x", f.h)
}

// RunIncrementalAnalysis (subflow-scoped partial re-analysis) was removed as
// dead code: it was never wired into any caller, and the cache in cache.go
// (content hash + settings digest) already makes full re-analysis cheap.
