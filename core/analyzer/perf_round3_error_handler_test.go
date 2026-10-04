package analyzer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"pad-core/models"
)

// naiveHasErrorHandler is the ancestor walk computeHasErrorHandler is defined
// by, written against the block tree itself (parent.Children) rather than any
// RuleContext index, so it is an independent oracle: a block has an error
// handler iff some ancestor is itself an error handler or has an error-handler
// among its own children (the block's siblings, or an ancestor's siblings).
// Top-level handlers never count: a top-level block has no ancestor, and a
// nested block's ancestor chain stops at the top-level block, whose own
// siblings are not inspected.
func naiveHasErrorHandler(ctx *RuleContext, id string) bool {
	cur := id
	for {
		pid, ok := ctx.ParentMap[cur]
		if !ok {
			return false
		}
		parent := ctx.AllBlocks[pid]
		if parent == nil {
			return false
		}
		if parent.Type == models.BlockTypeErrorHandler {
			return true
		}
		for i := range parent.Children {
			if parent.Children[i].Type == models.BlockTypeErrorHandler {
				return true
			}
		}
		cur = pid
	}
}

func r3Block(id string, bt models.BlockType, rawType string, children ...models.Block) models.Block {
	b := makeBlock(id, id, bt, rawType, 0)
	b.SubflowID = "sf1"
	if len(children) > 0 {
		b.Children = children
	}
	return *b
}

func r3Action(id string, children ...models.Block) models.Block {
	return r3Block(id, models.BlockTypeAction, "WebAutomation.Click.Click", children...)
}

func r3Handler(id string, children ...models.Block) models.Block {
	return r3Block(id, models.BlockTypeErrorHandler, "ON BLOCK ERROR", children...)
}

func r3If(id string, children ...models.Block) models.Block {
	return r3Block(id, models.BlockTypeCondition, "IF", children...)
}

// buildErrorHandlerFixture covers every ancestor shape the flag is defined
// over, each labelled with the value the oracle must produce for it.
func buildErrorHandlerFixture() (*models.FlowDocument, map[string]bool) {
	want := map[string]bool{}

	// (a) handler as parent: children of a handler, at any depth below it.
	parentCase := r3Handler("pa-handler",
		r3Action("pa-child"),
		r3If("pa-if", r3Action("pa-grandchild")),
	)
	want["pa-handler"] = false // top-level, no ancestor
	want["pa-child"] = true
	want["pa-if"] = true
	want["pa-grandchild"] = true

	// (b) handler as a sibling of an ancestor: the handler sits beside the
	// block's grandparent, inside a top-level container.
	siblingCase := r3If("sa-outer",
		r3If("sa-grandparent",
			r3If("sa-parent",
				r3Action("sa-block"),
			),
		),
		r3Handler("sa-handler"),
	)
	want["sa-outer"] = false
	want["sa-grandparent"] = true // its own siblings include the handler
	want["sa-parent"] = true
	want["sa-block"] = true
	want["sa-handler"] = true // a handler's siblings include itself

	// (c) handler only in a cousin group: the handler is under one branch,
	// the block under another; no ancestor of the block sees it.
	cousinCase := r3If("co-outer",
		r3If("co-left", r3Handler("co-handler"), r3Action("co-left-child")),
		r3If("co-right", r3Action("co-block"), r3If("co-right-if", r3Action("co-deep"))),
	)
	want["co-outer"] = false
	want["co-left"] = false
	want["co-handler"] = true
	want["co-left-child"] = true
	want["co-right"] = false
	want["co-block"] = false
	want["co-right-if"] = false
	want["co-deep"] = false

	// (d) no handler anywhere in the chain.
	noneCase := r3If("no-outer", r3If("no-inner", r3Action("no-block")))
	want["no-outer"] = false
	want["no-inner"] = false
	want["no-block"] = false

	// (e) top-level blocks, including a top-level handler beside a container:
	// top-level siblings are never inspected, so the container's children
	// stay false (the group key for that scan would be the subflow ID, which
	// the per-block ancestor walk never reaches).
	topHandler := r3Handler("top-handler", r3Action("top-handler-child"))
	topContainer := r3If("top-container", r3Action("top-container-child"))
	want["top-handler"] = false
	want["top-handler-child"] = true
	want["top-container"] = false
	want["top-container-child"] = false

	flow := &models.FlowDocument{ID: "eh", Subflows: []models.Subflow{{
		ID: "sf1", Name: "Main",
		Blocks: []models.Block{parentCase, siblingCase, cousinCase, noneCase, topHandler, topContainer},
	}}}
	return flow, want
}

// TestHasErrorHandler_GroupFlagMatchesAncestorWalk pins that the per-group
// GroupHasHandler flag computeHasErrorHandler now reads gives, for every
// block, exactly what the old per-block sibling rescan gave: the oracle
// walks parent.Children directly.
func TestHasErrorHandler_GroupFlagMatchesAncestorWalk(t *testing.T) {
	flow, want := buildErrorHandlerFixture()
	ctx := buildContext(flow, nil)

	if len(ctx.AllBlocks) != len(want) {
		t.Fatalf("fixture has %d blocks, expectations cover %d", len(ctx.AllBlocks), len(want))
	}
	for id := range ctx.AllBlocks {
		got, ok := ctx.HasErrorHandler[id]
		if !ok {
			t.Errorf("%s: no HasErrorHandler entry", id)
			continue
		}
		if oracle := naiveHasErrorHandler(ctx, id); got != oracle {
			t.Errorf("%s: HasErrorHandler = %v, ancestor walk = %v", id, got, oracle)
		}
		if exp, ok := want[id]; !ok {
			t.Errorf("%s: fixture block has no expectation", id)
		} else if got != exp {
			t.Errorf("%s: HasErrorHandler = %v, want %v", id, got, exp)
		}
	}

	// The group flag itself: keyed by the parent's ID for nested groups and
	// the subflow ID for the top-level group.
	for key, exp := range map[string]bool{
		"sf1":      true, // top-handler and pa-handler are top-level
		"sa-outer": true,
		"co-left":  true,
		"co-right": false,
		"no-outer": false,
	} {
		if got := ctx.GroupHasHandler[key]; got != exp {
			t.Errorf("GroupHasHandler[%q] = %v, want %v", key, got, exp)
		}
	}
}

// wideChildrenBudget separates O(children) from O(children²) at
// wideChildrenCount: the per-block sibling rescan took ~8 s here, the
// per-group flag well under a second, so the bound does not flake on a
// loaded machine (or under -race) and still fails unmistakably on a
// regression.
const (
	wideChildrenBudget = 1500 * time.Millisecond
	wideChildrenCount  = 60_000
)

// buildWideContainerFlow builds one container block with n children, the
// shape where every child's HasErrorHandler computation rescanned the whole
// sibling group.
func buildWideContainerFlow(n int) *models.FlowDocument {
	kids := make([]models.Block, 0, n)
	for i := range n {
		b := makeBlock(fmt.Sprintf("c%d", i), fmt.Sprintf("Click %d", i), models.BlockTypeAction, "WebAutomation.Click.Click", 1)
		b.SubflowID = "sf1"
		b.LineNumber = i + 2
		kids = append(kids, *b)
	}
	loop := makeBlock("loop", "Loop 1 to 10", models.BlockTypeLoop, "Loop.Loop", 0)
	loop.SubflowID = "sf1"
	loop.LineNumber = 1
	loop.Children = kids
	return &models.FlowDocument{ID: "wide", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
}

// TestHasErrorHandler_WideContainerIsLinear is the regression gate for the
// s² sibling rescan in computeHasErrorHandler: buildContext (via the
// zero-rule analysis path) over one block with wideChildrenCount children
// must finish inside wideChildrenBudget.
func TestHasErrorHandler_WideContainerIsLinear(t *testing.T) {
	flow := buildWideContainerFlow(wideChildrenCount)

	done := make(chan *models.AnalysisReport, 1)
	start := time.Now()
	go func() { done <- runAnalysisCore(context.Background(), flow, nil, nil, nil, false) }()

	select {
	case report := <-done:
		t.Logf("zero-rule analysis of %d children took %v", wideChildrenCount, time.Since(start))
		if report == nil {
			t.Fatal("runAnalysisCore returned nil")
		}
	case <-time.After(wideChildrenBudget):
		t.Fatalf("analysis of one block with %d children did not finish within %v — "+
			"computeHasErrorHandler is rescanning the sibling group per block again", wideChildrenCount, wideChildrenBudget)
	}
}
