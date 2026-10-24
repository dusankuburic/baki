package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-core/models"
)

// R-approvals-3: the decision wait and the apply + verify window used to share one 120 s tool deadline, so a slow
// approval left the write too little time. The applier now starts the apply window when the user approves and
// sizes it per item (fixApplyTimeout); a verification that runs out of that window reports the fix as applied but
// not verified, rather than as unresolved.

// shrinkApplyWindow sets the apply window for one test.
func shrinkApplyWindow(t *testing.T, base, perItem time.Duration) {
	t.Helper()
	oldBase, oldPer := fixApplyBase, fixApplyPerItem
	fixApplyBase, fixApplyPerItem = base, perItem
	t.Cleanup(func() { fixApplyBase, fixApplyPerItem = oldBase, oldPer })
}

// approveStreamAfter approves every pending decision of streamID once it has waited delay.
func approveStreamAfter(svc *ChatService, streamID string, delay time.Duration) {
	go func() {
		for i := 0; i < 2000; i++ {
			found := false
			svc.pendingFixDecisions.Range(func(k, v any) bool {
				if st := v.(*fixDecisionState); st.streamID == streamID {
					found = true
					time.Sleep(delay)
					_ = svc.ResolveFixDecision(streamID, k.(string), true, nil)
				}
				return false
			})
			if found {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
}

// fixedDocWithout returns a copy of doc where blockID is replaced by a comment (a fresh ID), as an applied
// wrap-error-handler fix re-parses the flow.
func fixedDocWithout(doc *models.FlowDocument, blockID string) *models.FlowDocument {
	fixed := &models.FlowDocument{ID: doc.ID, Name: doc.Name}
	for _, sf := range doc.Subflows {
		nsf := models.Subflow{ID: sf.ID, Name: sf.Name}
		for _, b := range sf.Blocks {
			if b.ID == blockID {
				b = models.Block{ID: "fixed-" + blockID, Name: "Comment", Type: models.BlockTypeAction, RawType: "Comment", LineNumber: b.LineNumber}
			}
			nsf.Blocks = append(nsf.Blocks, b)
		}
		fixed.Subflows = append(fixed.Subflows, nsf)
	}
	fixed.RebuildIndexes()
	return fixed
}

// tenCallDoc holds ten HTTP calls, each with an unhandled-error finding.
func tenCallDoc() (*models.FlowDocument, []ai.FixProposal) {
	doc := &models.FlowDocument{ID: "flow-1", Name: "Main"}
	sf := models.Subflow{ID: "sf-main", Name: "Main"}
	props := make([]ai.FixProposal, 0, 10)
	for i := 0; i < 10; i++ {
		id, name := fmt.Sprintf("b%d", i), fmt.Sprintf("Call API %d", i)
		sf.Blocks = append(sf.Blocks, models.Block{ID: id, Name: name, Type: models.BlockTypeAction, RawType: "HTTPClient.InvokeUrl", LineNumber: i + 1,
			Properties: map[string]string{"Url": fmt.Sprintf("https://x/%d", i)}})
		props = append(props, ai.FixProposal{ProposalID: "prop-" + id, RuleID: "unhandled-error", FixType: "wrap-error-handler", BlockID: id, BlockLabel: name, Line: i + 1})
	}
	doc.Subflows = []models.Subflow{sf}
	doc.RebuildIndexes()
	return doc, props
}

func TestApplyFixWithApproval_ApplyWindowStartsAtApproval(t *testing.T) {
	shrinkApplyWindow(t, 200*time.Millisecond, 300*time.Millisecond)
	doc := fixTestDoc()
	var (
		mu       sync.Mutex
		left     time.Duration
		deadline bool
	)
	svc, _ := newFixTestService(doc, func(ctx context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		mu.Lock()
		var dl time.Time
		dl, deadline = ctx.Deadline()
		left = time.Until(dl)
		mu.Unlock()
		return fixedDocWithout(d, blockID), nil
	})
	rec := &fixEventRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	// A slow decision, longer than the whole apply window: it must not eat into the write.
	approveStreamAfter(svc, "stream-1", 600*time.Millisecond)

	out, err := ap.ApplyFixWithApproval(context.Background(), fixTestProposal())
	if err != nil {
		t.Fatalf("ApplyFixWithApproval: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deadline {
		t.Fatalf("the apply ran without a deadline of its own (result %q)", out)
	}
	if want := fixApplyTimeout(1); left <= want-150*time.Millisecond || left > want {
		t.Errorf("the apply had %s left, want about %s from the approval", left, want)
	}
	if !strings.HasPrefix(out, "APPLIED") {
		t.Errorf("result = %q, want APPLIED", out)
	}
}

func TestApplyFixWithApproval_VerificationOutOfTimeIsAppliedNotVerified(t *testing.T) {
	shrinkApplyWindow(t, 0, 80*time.Millisecond)
	doc := fixTestDoc()
	// The write lands as the window closes, so the verifying analysis has no time left.
	svc, _ := newFixTestService(doc, func(ctx context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		select {
		case <-ctx.Done():
		case <-time.After(300 * time.Millisecond):
		}
		return fixedDocWithout(d, blockID), nil
	})
	rec := &fixEventRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	prop := fixTestProposal()
	prop.Fingerprint = realFingerprintFor(t, svc, doc, prop.RuleID, prop.BlockID)
	approveStreamAfter(svc, "stream-1", 0)

	out, err := ap.ApplyFixWithApproval(context.Background(), prop)
	if err != nil {
		t.Fatalf("ApplyFixWithApproval: %v", err)
	}
	if !strings.HasPrefix(out, "APPLIED, not verified") || !strings.Contains(out, "verification was skipped") {
		t.Errorf("result = %q, want the fix reported applied and not verified", out)
	}
	if !strings.Contains(out, "ALL block IDs have changed") {
		t.Errorf("an unverified apply re-parsed the flow too; the result must say the block IDs changed: %q", out)
	}
	if evs := rec.snapshot(); len(evs) == 0 || evs[len(evs)-1] != "fix_decision:applied" {
		t.Errorf("events = %v, want the card resolved as applied", evs)
	}
}

func TestApplyFixesWithApproval_TenSlowItemsAllComplete(t *testing.T) {
	shrinkApplyWindow(t, 100*time.Millisecond, 150*time.Millisecond)
	doc, props := tenCallDoc()
	var (
		mu    sync.Mutex
		first time.Duration
		calls int
	)
	// Each apply takes 60 ms: the batch needs ~600 ms plus analyses, far more than a one-item window.
	svc, _ := newFixTestService(doc, func(ctx context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		mu.Lock()
		if calls == 0 {
			if dl, ok := ctx.Deadline(); ok {
				first = time.Until(dl)
			}
		}
		calls++
		mu.Unlock()
		time.Sleep(60 * time.Millisecond)
		return fixedDocWithout(d, blockID), nil
	})
	rec := &fixEventRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	approveStreamAfter(svc, "stream-1", 300*time.Millisecond)

	out, err := ap.ApplyFixesWithApproval(context.Background(), props)
	if err != nil {
		t.Fatalf("ApplyFixesWithApproval: %v", err)
	}
	if !strings.HasPrefix(out, "Batch result: applied.") || strings.Count(out, ": applied") != 11 {
		t.Errorf("not every item completed:\n%s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := fixApplyTimeout(10); first <= want-300*time.Millisecond || first > want {
		t.Errorf("the batch's apply window had %s left at the first apply, want about %s (sized per item)", first, want)
	}
}

func TestApplyFixesWithApproval_OutOfTimeItemsAreNotAttempted(t *testing.T) {
	shrinkApplyWindow(t, 0, 25*time.Millisecond)
	doc, props := tenCallDoc()
	props = props[:4]
	svc, _ := newFixTestService(doc, func(ctx context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		// The first apply lands as the 100 ms window closes; the rest of the batch is out of time.
		select {
		case <-ctx.Done():
		case <-time.After(400 * time.Millisecond):
		}
		return fixedDocWithout(d, blockID), nil
	})
	rec := &fixEventRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	approveStreamAfter(svc, "stream-1", 0)

	out, err := ap.ApplyFixesWithApproval(context.Background(), props)
	if err != nil {
		t.Fatalf("ApplyFixesWithApproval: %v", err)
	}
	if !strings.Contains(out, "not attempted: the batch ran out of time") {
		t.Errorf("later items must be reported as not attempted for lack of time:\n%s", out)
	}
	if strings.Contains(out, "re-analysis failed mid-batch") {
		t.Errorf("an out-of-time item must not read as an analysis failure:\n%s", out)
	}
}
