package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// newReassocService is newFixTestService with the document provider exposed, so a test can replace the flow
// between the proposal and the decision as a desktop save or a collaborator's edit does.
func newReassocService(doc *models.FlowDocument, hook applyFixFunc) (*ChatService, *LocalDocumentProvider) {
	notifier := &testutil.CountingNotifier{}
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	analysisSvc, err := NewAnalysisService(notifier, nilSettings{}, nil, AnalysisOptions{})
	if err != nil {
		panic(err)
	}
	return &ChatService{
		notifier:      notifier,
		flowCache:     NewFlowService(notifier, nil, ldp, nil, nil, nil),
		analysisCache: analysisSvc,
		applyFixFunc:  hook,
	}, ldp
}

// reparsed is doc as a re-parse returns it: the same content under new block ids.
func reparsed(doc *models.FlowDocument, prefix string) *models.FlowDocument {
	out := &models.FlowDocument{ID: doc.ID, Name: doc.Name}
	for _, sf := range doc.Subflows {
		nsf := models.Subflow{ID: sf.ID, Name: sf.Name}
		for _, b := range sf.Blocks {
			b.ID = prefix + b.ID
			nsf.Blocks = append(nsf.Blocks, b)
		}
		out.Subflows = append(out.Subflows, nsf)
	}
	out.RebuildIndexes()
	return out
}

// proposalFor builds the proposal the fix tool would: the block's anchor taken on the document it was proposed on.
func proposalFor(t *testing.T, svc *ChatService, doc *models.FlowDocument, id, blockID string) ai.FixProposal {
	t.Helper()
	b := doc.BlocksByID[blockID]
	if b == nil {
		t.Fatalf("no block %s", blockID)
	}
	return ai.FixProposal{
		ProposalID: id, RuleID: "unhandled-error", FixType: "wrap-error-handler", BlockID: blockID, BlockLabel: b.Name,
		Line: b.LineNumber, Fingerprint: realFingerprintFor(t, svc, doc, "unhandled-error", blockID), Anchor: doc.AnchorOf(blockID),
	}
}

// approveWhenShown approves proposal id once it is pending, after running before (the edit that lands during
// the approval wait).
func approveWhenShown(svc *ChatService, id string, before func()) {
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, ok := svc.pendingFixDecisions.Load(id); ok {
				if before != nil {
					before()
				}
				_ = svc.ResolveFixDecision("stream-1", id, true, nil)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
}

// approveBatchWhenShown approves the stream's first pending card, after running before.
func approveBatchWhenShown(svc *ChatService, before func()) {
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			done := false
			svc.pendingFixDecisions.Range(func(k, _ any) bool {
				if before != nil {
					before()
				}
				_ = svc.ResolveFixDecision("stream-1", k.(string), true, nil)
				done = true
				return false
			})
			if done {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
}

// idHook applies only to block ids the current document has (as generateFixPatch does) and returns fixed.
type idHook struct {
	mu    sync.Mutex
	fixed func() *models.FlowDocument
	calls []string
	errs  []error // returned in order before any success
}

func (h *idHook) apply(_ context.Context, doc *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, blockID)
	if len(h.errs) > 0 {
		err := h.errs[0]
		h.errs = h.errs[1:]
		return nil, err
	}
	if doc.BlocksByID[blockID] == nil {
		return nil, fmt.Errorf("block %q not found", blockID)
	}
	return h.fixed(), nil
}

func (h *idHook) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// lastDecision returns the card's terminal fix_decision data.
type decisionRecorder struct {
	mu     sync.Mutex
	events []map[string]interface{}
}

func (r *decisionRecorder) emit(typ string, data map[string]interface{}) {
	if typ != "fix_decision" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, data)
}

func (r *decisionRecorder) last() map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return nil
	}
	return r.events[len(r.events)-1]
}

func commentDoc(id string) *models.FlowDocument {
	d := &models.FlowDocument{ID: id, Name: "Main"}
	d.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: []models.Block{
		{ID: "done-1", Name: "Comment", Type: models.BlockTypeAction, RawType: "Comment", LineNumber: 1},
	}}}
	d.RebuildIndexes()
	return d
}

// TestApplyFix_ReassociatesAfterAnEditDuringTheWait (R-approvals-2): the flow is re-parsed while the card waits
// (new block ids, same content); the approved fix lands on the block where it is now and is applied.
func TestApplyFix_ReassociatesAfterAnEditDuringTheWait(t *testing.T) {
	doc := fixTestDoc()
	h := &idHook{fixed: func() *models.FlowDocument { return commentDoc(doc.ID) }}
	svc, ldp := newReassocService(doc, h.apply)
	prop := proposalFor(t, svc, doc, "prop-r1", "b1")
	rec := &decisionRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	approveWhenShown(svc, prop.ProposalID, func() { ldp.SetCurrentDoc(reparsed(doc, "new-")) })

	out, err := ap.ApplyFixWithApproval(context.Background(), prop)
	if err != nil {
		t.Fatalf("ApplyFixWithApproval: %v", err)
	}
	if st := rec.last()["status"]; st != "applied" {
		t.Errorf("decision status = %v (%v), want applied; result %q", st, rec.last()["message"], out)
	}
	if calls := h.seen(); len(calls) != 1 || calls[0] != "new-b1" {
		t.Errorf("apply hook calls = %v, want [new-b1] (the block's id in the edited flow)", calls)
	}
}

// TestApplyFix_FindingGoneBeforeApprovalIsAlreadyResolved: the finding is gone by the time the user approves;
// nothing is applied and the card says so, not "error".
func TestApplyFix_FindingGoneBeforeApprovalIsAlreadyResolved(t *testing.T) {
	doc := fixTestDoc()
	h := &idHook{fixed: func() *models.FlowDocument { return commentDoc(doc.ID) }}
	svc, ldp := newReassocService(doc, h.apply)
	prop := proposalFor(t, svc, doc, "prop-r2", "b1")
	rec := &decisionRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	approveWhenShown(svc, prop.ProposalID, func() { ldp.SetCurrentDoc(commentDoc(doc.ID)) })

	out, err := ap.ApplyFixWithApproval(context.Background(), prop)
	if err != nil {
		t.Fatalf("ApplyFixWithApproval: %v", err)
	}
	if st := rec.last()["status"]; st != "already-resolved" {
		t.Errorf("decision status = %v, want already-resolved", st)
	}
	if len(h.seen()) != 0 {
		t.Errorf("the apply hook ran for a finding that is gone: %v", h.seen())
	}
	if strings.HasPrefix(out, "error") || !strings.Contains(out, "no longer appears") {
		t.Errorf("model result = %q, want it to say the finding is gone", out)
	}
}

// TestApplyFix_VersionConflictIsRetriedOnce: a concurrent save between the version read and the write is
// retried once on a fresh read; the second attempt applies.
func TestApplyFix_VersionConflictIsRetriedOnce(t *testing.T) {
	doc := fixTestDoc()
	h := &idHook{fixed: func() *models.FlowDocument { return commentDoc(doc.ID) }, errs: []error{storageif.ErrVersionConflict}}
	svc, _ := newReassocService(doc, h.apply)
	prop := proposalFor(t, svc, doc, "prop-r3", "b1")
	rec := &decisionRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	approveWhenShown(svc, prop.ProposalID, nil)

	if _, err := ap.ApplyFixWithApproval(context.Background(), prop); err != nil {
		t.Fatalf("ApplyFixWithApproval: %v", err)
	}
	if st := rec.last()["status"]; st != "applied" {
		t.Errorf("decision status = %v (%v), want applied after one retry", st, rec.last()["message"])
	}
	if n := len(h.seen()); n != 2 {
		t.Errorf("apply attempts = %d, want 2", n)
	}
}

// TestApplyFix_ApplyFailuresAreCurated: two conflicts in a row, or any other apply error, resolve the card as
// error with text the model can act on — no raw storage text, no "error:" tool result, and no stream error.
func TestApplyFix_ApplyFailuresAreCurated(t *testing.T) {
	for name, errs := range map[string][]error{
		"two conflicts": {storageif.ErrVersionConflict, storageif.ErrVersionConflict},
		"storage error": {errors.New("pq: could not write row 42 in relation flows: disk quota exceeded")},
	} {
		t.Run(name, func(t *testing.T) {
			doc := fixTestDoc()
			h := &idHook{fixed: func() *models.FlowDocument { return commentDoc(doc.ID) }, errs: errs}
			svc, _ := newReassocService(doc, h.apply)
			prop := proposalFor(t, svc, doc, "prop-r4-"+strings.ReplaceAll(name, " ", "-"), "b1")
			rec := &decisionRecorder{}
			ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
			approveWhenShown(svc, prop.ProposalID, nil)

			out, err := ap.ApplyFixWithApproval(context.Background(), prop)
			if err != nil {
				t.Fatalf("ApplyFixWithApproval returned %v; an apply failure is the model's to read, not the stream's", err)
			}
			last := rec.last()
			msg, _ := last["message"].(string)
			if last["status"] != "error" {
				t.Errorf("decision status = %v, want error", last["status"])
			}
			for _, raw := range []string{"version conflict", "pq:", "relation flows"} {
				if strings.Contains(msg, raw) || strings.Contains(out, raw) {
					t.Errorf("raw storage text %q reached the card (%q) or the model (%q)", raw, msg, out)
				}
			}
			if strings.HasPrefix(out, "error") || out == "" {
				t.Errorf("model result = %q, want a curated outcome", out)
			}
		})
	}
}

// threeCallDoc carries three HTTP blocks the parser names alike ("Invoke Url"): identical siblings, told apart
// only by their place.
func threeCallDoc(id string) *models.FlowDocument {
	d := &models.FlowDocument{ID: id, Name: "Main"}
	var blocks []models.Block
	for i, bid := range []string{"b1", "b2", "b3"} {
		blocks = append(blocks, models.Block{ID: bid, Name: "Invoke Url", Type: models.BlockTypeAction, RawType: "HTTPClient.InvokeUrl",
			LineNumber: 3 + 2*i, Properties: map[string]string{"Url": "https://x" + bid}})
	}
	d.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: blocks}}
	d.RebuildIndexes()
	return d
}

// withoutBlocks is doc as a re-parse returns it after wrap-error-handler fixed the blocks named by fix: every id
// re-minted with prefix, each fixed block moved inside an On Block Error handler (it keeps its type and name, so
// its siblings still count it), the handled ones staying handled.
func withoutBlocks(doc *models.FlowDocument, prefix string, fix ...string) *models.FlowDocument {
	wrap := map[string]bool{}
	for _, id := range fix {
		wrap[id] = true
	}
	var remint func(b models.Block) models.Block
	remint = func(b models.Block) models.Block {
		wrapped := wrap[b.ID]
		b.ID = prefix + b.ID
		kids := b.Children
		b.Children = nil
		for _, k := range kids {
			b.Children = append(b.Children, remint(k))
		}
		if wrapped {
			return models.Block{ID: prefix + "h-" + b.ID, Name: "On Block Error", Type: models.BlockTypeErrorHandler, RawType: "OnBlockError",
				LineNumber: b.LineNumber, Children: []models.Block{b}}
		}
		return b
	}
	out := &models.FlowDocument{ID: doc.ID, Name: doc.Name}
	for _, sf := range doc.Subflows {
		nsf := models.Subflow{ID: sf.ID, Name: sf.Name}
		for _, b := range sf.Blocks {
			nsf.Blocks = append(nsf.Blocks, remint(b))
		}
		out.Subflows = append(out.Subflows, nsf)
	}
	out.RebuildIndexes()
	return out
}

// TestApplyFixes_ReassociatesIdenticalSiblingsAfterAnEdit (R-approvals-2): the flow is re-parsed while the batch
// card waits, and the user deselected the middle of three identical siblings. Each approved target is applied on
// its own block — found by its place, not by a name its siblings share — and none is reported already resolved.
func TestApplyFixes_ReassociatesIdenticalSiblingsAfterAnEdit(t *testing.T) {
	doc := threeCallDoc("flow-1")
	var cur *models.FlowDocument
	var mu sync.Mutex
	var calls []string
	var ldp *LocalDocumentProvider
	hook := func(_ context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		mu.Lock()
		defer mu.Unlock()
		if d.BlocksByID[blockID] == nil {
			return nil, fmt.Errorf("block %q not found", blockID)
		}
		calls = append(calls, blockID)
		cur = withoutBlocks(d, fmt.Sprintf("p%d-", len(calls)), blockID)
		return cur, nil
	}
	svc, l := newReassocService(doc, hook)
	ldp = l
	props := []ai.FixProposal{
		proposalFor(t, svc, doc, "p1", "b1"), proposalFor(t, svc, doc, "p2", "b2"), proposalFor(t, svc, doc, "p3", "b3"),
	}
	rec := &decisionRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	edited := reparsed(doc, "e-")
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			done := false
			svc.pendingFixDecisions.Range(func(k, _ any) bool {
				ldp.SetCurrentDoc(edited)
				_ = svc.ResolveFixDecision("stream-1", k.(string), true, []int{1})
				done = true
				return false
			})
			if done {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	if _, err := ap.ApplyFixesWithApproval(context.Background(), props); err != nil {
		t.Fatalf("ApplyFixesWithApproval: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "e-b1" || got[1] != "p1-e-b3" {
		t.Errorf("apply hook calls = %v, want [e-b1 p1-e-b3]: the first and third block, never the deselected second", got)
	}
	items, _ := rec.last()["items"].([]map[string]interface{})
	want := []string{"applied", "skipped", "applied"}
	if len(items) != len(want) {
		t.Fatalf("decision items = %v, want statuses %v", items, want)
	}
	for i, w := range want {
		if items[i]["status"] != w {
			t.Errorf("item %d = %v, want %s", i, items[i], w)
		}
	}
}

// TestApplyFixes_ConflictRetriesTheItem: a batch item whose write hits a version conflict is retried once on its
// own and reported per item; a second conflict fails only that item, in curated words.
func TestApplyFixes_ConflictRetriesTheItem(t *testing.T) {
	doc := threeCallDoc("flow-1")
	var mu sync.Mutex
	attempts := map[string]int{}
	var ldp *LocalDocumentProvider
	// Like FlowService.ApplyFix, a successful apply saves the flow: a re-read after a conflict sees the earlier fixes.
	hook := func(_ context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		mu.Lock()
		defer mu.Unlock()
		key := strings.TrimLeft(blockID, "p0123456789-")
		attempts[key]++
		switch {
		case key == "b1" && attempts[key] == 1:
			return nil, storageif.ErrVersionConflict
		case key == "b3":
			return nil, storageif.ErrVersionConflict
		}
		fixed := withoutBlocks(d, fmt.Sprintf("p%d-", attempts[key]), blockID)
		ldp.SetCurrentDoc(fixed)
		return fixed, nil
	}
	svc, l := newReassocService(doc, hook)
	ldp = l
	props := []ai.FixProposal{
		proposalFor(t, svc, doc, "p1", "b1"), proposalFor(t, svc, doc, "p2", "b2"), proposalFor(t, svc, doc, "p3", "b3"),
	}
	rec := &decisionRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	approveBatchWhenShown(svc, nil)

	out, err := ap.ApplyFixesWithApproval(context.Background(), props)
	if err != nil {
		t.Fatalf("ApplyFixesWithApproval: %v", err)
	}
	items, _ := rec.last()["items"].([]map[string]interface{})
	want := []string{"applied", "applied", "error"}
	if len(items) != len(want) {
		t.Fatalf("decision items = %v, want statuses %v", items, want)
	}
	for i, w := range want {
		if items[i]["status"] != w {
			t.Errorf("item %d = %v, want %s", i, items[i], w)
		}
		if m, _ := items[i]["message"].(string); strings.Contains(m, "version conflict") {
			t.Errorf("item %d carries raw storage text: %q", i, m)
		}
	}
	if strings.Contains(out, "version conflict") {
		t.Errorf("the model read raw storage text: %q", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts["b1"] != 2 || attempts["b3"] != 2 {
		t.Errorf("attempts = %v, want two for b1 (conflict, then applied) and two for b3 (two conflicts)", attempts)
	}
}
