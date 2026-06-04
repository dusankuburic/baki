package analyzer

import (
	"fmt"
	"sort"
	"time"

	"pad-analyzer/internal/models"
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
	{FromRuleID: "sensitive-data-exposure", ToRuleID: "hardcoded-credential", Reason: "sensitive exposure may involve hardcoded credentials"},
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
			if color[next] == gray {
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
			} else if color[next] == white {
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
		f.h ^= uint32(c)
		f.h *= 16777619
	}
}

func (f *fnvHasher) sum() string {
	return fmt.Sprintf("%08x", f.h)
}

func RunIncrementalAnalysis(doc *models.FlowDocument, rules []Rule, settings *models.AppSettings, changedSubflows map[string]bool) *models.AnalysisReport {
	if len(changedSubflows) == 0 {
		return &models.AnalysisReport{
			FlowID:    doc.ID,
			Findings:  []models.Finding{},
			Stats:     models.AnalysisStats{},
		}
	}

	ctx := buildContext(doc, settings)

	var enabledRules []Rule
	for _, r := range rules {
		if settings != nil {
			if rc, ok := settings.Analysis.Rules[r.ID()]; ok && !rc.Enabled {
				continue
			}
		}
		enabledRules = append(enabledRules, r)
	}

	var findings []models.Finding
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		if !changedSubflows[sf.ID] {
			continue
		}
		walkSubflowBlocks(sf, func(block *models.Block) {
			if block.Type == models.BlockTypeEnd {
				return
			}
			for _, rule := range enabledRules {
				f := safeCheck(rule, block, ctx)
				for j := range f {
					f[j].Category = rule.Category()
				}
				findings = append(findings, f...)
			}
		})
	}

	if findings == nil {
		findings = []models.Finding{}
	}

	stats := computeStats(findings)
	stats.BlocksAnalyzed = countBlocks(doc, changedSubflows)
	stats.RulesRun = len(enabledRules)

	return &models.AnalysisReport{
		FlowID:      doc.ID,
		GeneratedAt: time.Now(),
		Findings:    findings,
		Stats:       stats,
	}
}

func countBlocks(doc *models.FlowDocument, subflows map[string]bool) int {
	count := 0
	for i := range doc.Subflows {
		if !subflows[doc.Subflows[i].ID] {
			continue
		}
		walkSubflowBlocks(&doc.Subflows[i], func(b *models.Block) {
			if b.Type != models.BlockTypeEnd {
				count++
			}
		})
	}
	return count
}
