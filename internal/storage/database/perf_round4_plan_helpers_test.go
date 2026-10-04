package database

// Plan-tree helpers for the fourth performance round's plan tests. planNode
// (perf_round2_plans_test.go) keeps its children, so the flattened list
// explainAsUser returns can still be walked as a tree from its first
// element.

// parentOf returns the parent of the first node (depth-first) for which pred
// holds, or nil when no such node exists or it is the root.
func parentOf(root planNode, pred func(planNode) bool) *planNode {
	var walk func(n planNode) *planNode
	walk = func(n planNode) *planNode {
		for _, c := range n.Plans {
			if pred(c) {
				return &n
			}
			if p := walk(c); p != nil {
				return p
			}
		}
		return nil
	}
	return walk(root)
}

// limitScope reports whether the plan has a Limit node and whether any scan
// of relation sits beneath one (i.e. is evaluated inside the LIMIT, per
// candidate row, rather than on the rows the LIMIT lets through).
func limitScope(root planNode, relation string) (limitSeen, underLimit bool) {
	var walk func(n planNode, inLimit bool)
	walk = func(n planNode, inLimit bool) {
		if n.NodeType == "Limit" {
			inLimit, limitSeen = true, true
		}
		if inLimit && n.RelationName == relation {
			underLimit = true
		}
		for _, c := range n.Plans {
			walk(c, inLimit)
		}
	}
	walk(root, false)
	return limitSeen, underLimit
}
