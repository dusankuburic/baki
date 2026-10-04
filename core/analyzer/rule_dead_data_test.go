package analyzer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"pad-core/models"
)

// r3Set builds a `SET name TO ...` block the way the parser does (RawType
// "SET", BlockTypeVariable, the written variable in _var).
func r3Set(id, name string) models.Block {
	b := makeBlock(id, "SET "+name, models.BlockTypeVariable, "SET", 0)
	b.SubflowID = "sf1"
	b.Properties = map[string]string{"_var": name, "_value": "1"}
	return *b
}

// r3Read builds a block that reads the given variables.
func r3Read(id string, vars ...string) models.Block {
	b := makeBlock(id, "Display message", models.BlockTypeAction, "Display.ShowMessage", 0)
	b.SubflowID = "sf1"
	b.Variables = vars
	return *b
}

func r3Exit(id string) models.Block {
	b := makeBlock(id, "EXIT", models.BlockTypeAction, "EXIT", 0)
	b.SubflowID = "sf1"
	return *b
}

// r3DeadDataFindings runs only the dead-data rule over flow and returns the
// block IDs it flagged, in report order.
func r3DeadDataFindings(t *testing.T, flow *models.FlowDocument) []string {
	t.Helper()
	report := runAnalysisCore(context.Background(), flow, []Rule{&DeadDataRule{}}, nil, nil, false)
	ids := make([]string, 0, len(report.Findings))
	for _, f := range report.Findings {
		if f.RuleID != "dead-data" {
			t.Fatalf("unexpected rule %q in findings", f.RuleID)
		}
		ids = append(ids, f.BlockID)
	}
	return ids
}

func r3Flow(blocks ...models.Block) *models.FlowDocument {
	for i := range blocks {
		blocks[i].LineNumber = i + 1
	}
	return &models.FlowDocument{ID: "dd", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}}}
}

// TestDeadDataRule_PrecomputedReaderClassification pins the rule's semantics
// now that readers are classified once per variable (AllReadersDead) instead
// of per writer.
func TestDeadDataRule_PrecomputedReaderClassification(t *testing.T) {
	t.Run("live reader before the terminator: no finding", func(t *testing.T) {
		got := r3DeadDataFindings(t, r3Flow(r3Set("w1", "X"), r3Read("r1", "X"), r3Exit("exit"), r3Read("r2", "X")))
		if len(got) != 0 {
			t.Fatalf("findings = %v, want none (r1 is live)", got)
		}
	})

	t.Run("self-writing reader is not a reader", func(t *testing.T) {
		// w2 reads X and writes X: excluded. With no other reader the
		// variable has zero readers, so w1 is not flagged...
		self := r3Set("w2", "X")
		self.Variables = []string{"X"}
		got := r3DeadDataFindings(t, r3Flow(r3Set("w1", "X"), self, r3Exit("exit")))
		if len(got) != 0 {
			t.Fatalf("findings = %v, want none (only reader is a self-write)", got)
		}
		// ...and a live-positioned self-write does not rescue a variable
		// whose only real reader is dead: both writers are flagged.
		got = r3DeadDataFindings(t, r3Flow(r3Set("w1", "X"), self, r3Exit("exit"), r3Read("r1", "X")))
		if fmt.Sprint(got) != "[w1 w2]" {
			t.Fatalf("findings = %v, want [w1 w2]", got)
		}
	})

	t.Run("zero readers: no finding", func(t *testing.T) {
		got := r3DeadDataFindings(t, r3Flow(r3Set("w1", "X"), r3Exit("exit"), r3Read("r1", "Y")))
		if len(got) != 0 {
			t.Fatalf("findings = %v, want none (nothing reads X)", got)
		}
	})

	t.Run("all readers after the terminator: one finding per writer", func(t *testing.T) {
		got := r3DeadDataFindings(t, r3Flow(
			r3Set("w1", "X"), r3Set("w2", "X"), r3Set("w3", "X"),
			r3Exit("exit"),
			r3Read("r1", "X"), r3Read("r2", "X"), r3Read("r3", "X", "X"),
		))
		if fmt.Sprint(got) != "[w1 w2 w3]" {
			t.Fatalf("findings = %v, want [w1 w2 w3]", got)
		}
	})

	t.Run("the terminator itself is a live reader", func(t *testing.T) {
		// The terminator is at the terminator index, not after it, so a
		// variable it reads is live even when every other reader is dead.
		exit := r3Exit("exit")
		exit.Variables = []string{"X"}
		got := r3DeadDataFindings(t, r3Flow(r3Set("w1", "X"), exit, r3Read("r1", "X")))
		if len(got) != 0 {
			t.Fatalf("findings = %v, want none (the EXIT block reads X)", got)
		}
	})

	t.Run("writer after its own terminator is not flagged", func(t *testing.T) {
		got := r3DeadDataFindings(t, r3Flow(r3Set("w1", "X"), r3Exit("exit"), r3Set("w2", "X"), r3Read("r1", "X")))
		if fmt.Sprint(got) != "[w1]" {
			t.Fatalf("findings = %v, want [w1]", got)
		}
	})

	t.Run("reader liveness is judged against its own sibling group", func(t *testing.T) {
		// The writer's group has a terminator; the reader lives in a nested
		// group. Dead there only if that group has a terminator before it.
		deadBranch := makeBlock("if-dead", "If true", models.BlockTypeCondition, "IF", 0)
		deadBranch.SubflowID = "sf1"
		deadBranch.Children = []models.Block{r3Exit("inner-exit"), r3Read("r-dead", "X")}
		got := r3DeadDataFindings(t, r3Flow(r3Set("w1", "X"), *deadBranch, r3Exit("exit")))
		if fmt.Sprint(got) != "[w1]" {
			t.Fatalf("findings = %v, want [w1] (nested reader is after its own group's exit)", got)
		}

		liveBranch := makeBlock("if-live", "If true", models.BlockTypeCondition, "IF", 0)
		liveBranch.SubflowID = "sf1"
		liveBranch.Children = []models.Block{r3Read("r-live", "X")}
		got = r3DeadDataFindings(t, r3Flow(r3Set("w1", "X"), *liveBranch, r3Exit("exit")))
		if len(got) != 0 {
			t.Fatalf("findings = %v, want none (nested reader's group has no terminator)", got)
		}
	})

	t.Run("no terminator anywhere: AllReadersDead is not built", func(t *testing.T) {
		ctx := buildContext(r3Flow(r3Set("w1", "X"), r3Read("r1", "X")), nil)
		if ctx.AllReadersDead != nil {
			t.Fatalf("AllReadersDead = %v, want nil without a terminator", ctx.AllReadersDead)
		}
		if got := (&DeadDataRule{}).Check(ctx.AllBlocks["w1"], ctx); len(got) != 0 {
			t.Fatalf("findings = %v, want none", got)
		}
	})
}

// deadDataBudget separates O(writers + readers) from O(writers × readers)
// at deadDataFanout writers and readers of one variable: the per-writer
// reader walk took ~3 s here, the per-variable classification milliseconds.
const (
	deadDataBudget = time.Second
	deadDataFanout = 8_000
)

// buildDeadDataFanoutFlow builds `SET X TO 1` × w, one EXIT, then w readers
// of X — every writer is live, every reader dead, so the rule flags all w
// writers and, before the fix, re-walked all r readers for each.
func buildDeadDataFanoutFlow(w, r int) *models.FlowDocument {
	blocks := make([]models.Block, 0, w+1+r)
	for i := range w {
		blocks = append(blocks, r3Set(fmt.Sprintf("w%d", i), "X"))
	}
	blocks = append(blocks, r3Exit("exit"))
	for i := range r {
		blocks = append(blocks, r3Read(fmt.Sprintf("r%d", i), "X"))
	}
	return r3Flow(blocks...)
}

// TestDeadDataRule_FanoutIsLinear is the regression gate for the per-writer
// findReaders walk in DeadDataRule.Check.
func TestDeadDataRule_FanoutIsLinear(t *testing.T) {
	flow := buildDeadDataFanoutFlow(deadDataFanout, deadDataFanout)

	done := make(chan *models.AnalysisReport, 1)
	start := time.Now()
	go func() { done <- runAnalysisCore(context.Background(), flow, []Rule{&DeadDataRule{}}, nil, nil, false) }()

	select {
	case report := <-done:
		t.Logf("dead-data over %d writers × %d readers took %v", deadDataFanout, deadDataFanout, time.Since(start))
		if got := len(report.Findings); got != deadDataFanout {
			t.Fatalf("findings = %d, want one per writer (%d)", got, deadDataFanout)
		}
	case <-time.After(deadDataBudget):
		t.Fatalf("dead-data over %d writers × %d readers did not finish within %v — "+
			"the rule is re-walking every reader per writer again", deadDataFanout, deadDataFanout, deadDataBudget)
	}
}
