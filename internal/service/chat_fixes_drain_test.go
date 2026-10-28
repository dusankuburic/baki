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
	"pad-analyzer/internal/platform/drain"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// R-approvals-c17: while the server drains no new approval is asked or approved, and an approved apply is a
// critical section the stop waits for.

// TestApprovals_DrainingAsksNothing: once the drain began, a fix tool shows no card and the model reads that
// nothing was changed.
func TestApprovals_DrainingAsksNothing(t *testing.T) {
	doc := fixTestDoc()
	svc, _ := newFixTestService(doc, failingApplyHook(t))
	svc.draining = drain.New()
	svc.draining.Begin()
	rec := &fixEventRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)

	for name, run := range map[string]func() (string, error){
		"single": func() (string, error) { return ap.ApplyFixWithApproval(context.Background(), fixTestProposal()) },
		"batch": func() (string, error) {
			return ap.ApplyFixesWithApproval(context.Background(), []ai.FixProposal{fixTestProposal()})
		},
	} {
		out, err := run()
		if err != nil || !strings.Contains(out, "restarting") || !strings.Contains(out, "nothing was changed") {
			t.Errorf("%s: out=%q err=%v, want the restart answer", name, out, err)
		}
	}
	if evs := rec.snapshot(); len(evs) != 0 {
		t.Errorf("a card was shown while draining: %v", evs)
	}
}

// TestResolveApproval_DrainingRefusesAnApprove: a card shown before the drain cannot be approved during it (the
// apply would be cut by the stop); a decline still resolves it.
func TestResolveApproval_DrainingRefusesAnApprove(t *testing.T) {
	svc, _ := newFixTestService(fixTestDoc(), nil)
	svc.draining = drain.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan approvalDecision, 1)
	go func() {
		dec, _ := svc.requestApproval(ctx, "stream-1", approvalRequest{id: "p-drain"}, func(string, map[string]interface{}) {}, nil)
		got <- dec
	}()
	waitPending(t, svc, "p-drain")
	svc.draining.Begin()

	if err := svc.ResolveApproval(context.Background(), "stream-1", "p-drain", true, nil); !errors.Is(err, ErrServerRestarting) {
		t.Fatalf("approve while draining: %v, want ErrServerRestarting", err)
	}
	if err := svc.ResolveApproval(context.Background(), "stream-1", "p-drain", false, nil); err != nil {
		t.Fatalf("decline while draining: %v", err)
	}
	select {
	case dec := <-got:
		if dec.approved {
			t.Error("the card resolved approved")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the decline did not resolve the card")
	}
}

func waitPending(t *testing.T, svc *ChatService, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := svc.pendingFixDecisions.Load(id); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("approval %s never registered", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// liveStream registers a running stream for the applier: its context ends only through the stream's cancel.
func liveStream(svc *ChatService, id string) (context.Context, *streamCtl) {
	ctx, cancel := context.WithCancel(context.Background())
	ctl := &streamCtl{cancel: cancel, streamID: id, started: make(chan struct{}), settled: make(chan struct{})}
	close(ctl.started)
	svc.activeStreams.Store(id, ctl)
	return ctx, ctl
}

// shutdownDuring runs the server's stop from inside an apply and waits until it asked the stream to end.
func shutdownDuring(t *testing.T, svc *ChatService, ctl *streamCtl) {
	t.Helper()
	go func() { _ = svc.Shutdown(context.Background()) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		ctl.mu.Lock()
		asked := ctl.cancelCode == ChatErrServerRestarting
		ctl.mu.Unlock()
		if asked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Shutdown never asked the stream to end")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestShutdown_WaitsForAnApprovedSingleApply: the stop arrives while the approved fix is being written; the write
// finishes on a live context, the card resolves applied, and only then does the stream end.
func TestShutdown_WaitsForAnApprovedSingleApply(t *testing.T) {
	doc := fixTestDoc()
	var svc *ChatService
	var ctl *streamCtl
	var ctxDuringWrite error
	hook := func(ctx context.Context, d *models.FlowDocument, _, _, _, _, _ string) (*models.FlowDocument, error) {
		shutdownDuring(t, svc, ctl)
		ctxDuringWrite = ctx.Err()
		return commentDoc(d.ID), nil
	}
	svc, _ = newFixTestService(doc, hook)
	svc.shutdownBudget = 5 * time.Second
	ctx, c := liveStream(svc, "stream-1")
	ctl = c
	rec := &decisionRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	prop := fixTestProposal()
	approveWhenShown(svc, prop.ProposalID, nil)

	out, _ := ap.ApplyFixWithApproval(ctx, prop)
	if ctxDuringWrite != nil {
		t.Errorf("the write ran on an ended context (%v): the stop did not wait for the apply", ctxDuringWrite)
	}
	if st := rec.last()["status"]; st != "applied" && st != "applied-unresolved" {
		t.Errorf("decision = %v, want the fix's real outcome; result %q", rec.last(), out)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Error("the stream was not ended once the apply returned")
	}
}

// TestShutdown_StopsABatchBetweenItems: the stop arrives during the second of five fixes; the first two report
// their real outcome, the other three are not attempted and say why, and no raw context error reaches the card or
// the model.
func TestShutdown_StopsABatchBetweenItems(t *testing.T) {
	doc := &models.FlowDocument{ID: "flow-1", Name: "Main"}
	var blocks []models.Block
	for i := 1; i <= 5; i++ {
		blocks = append(blocks, models.Block{ID: fmt.Sprintf("b%d", i), Name: fmt.Sprintf("Call %d", i), Type: models.BlockTypeAction,
			RawType: "HTTPClient.InvokeUrl", LineNumber: 2 * i, Properties: map[string]string{"Url": "https://x"}})
	}
	doc.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: blocks}}
	doc.RebuildIndexes()

	var svc *ChatService
	var ctl *streamCtl
	var mu sync.Mutex
	var calls []string
	hook := func(_ context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		mu.Lock()
		calls = append(calls, blockID)
		n := len(calls)
		mu.Unlock()
		if n == 2 {
			shutdownDuring(t, svc, ctl)
		}
		return withoutBlocks(d, fmt.Sprintf("p%d-", n), blockID), nil
	}
	svc, _ = newFixTestService(doc, hook)
	svc.shutdownBudget = 5 * time.Second
	ctx, c := liveStream(svc, "stream-1")
	ctl = c
	var props []ai.FixProposal
	for i := 1; i <= 5; i++ {
		props = append(props, proposalFor(t, svc, doc, fmt.Sprintf("p%d", i), fmt.Sprintf("b%d", i)))
	}
	var all []map[string]interface{}
	var emu sync.Mutex
	emit := func(typ string, data map[string]interface{}) {
		emu.Lock()
		defer emu.Unlock()
		if typ == "fix_decision" {
			all = append(all, data)
		}
	}
	ap := svc.newFixApplier(doc, "stream-1", emit, nil, nil)
	approveBatchWhenShown(svc, nil)

	out, _ := ap.ApplyFixesWithApproval(ctx, props)
	mu.Lock()
	if len(calls) != 2 {
		t.Errorf("fixes written = %v, want the first two only", calls)
	}
	mu.Unlock()
	emu.Lock()
	defer emu.Unlock()
	last := all[len(all)-1]
	items, _ := last["items"].([]map[string]interface{})
	if len(items) != 5 {
		t.Fatalf("decision items = %v, want five", items)
	}
	for i, it := range items {
		msg, _ := it["message"].(string)
		if i < 2 && it["status"] != "applied" {
			t.Errorf("item %d = %v, want applied", i, it)
		}
		if i >= 2 && (it["status"] != "error" || !strings.Contains(msg, "not attempted") || !strings.Contains(msg, "restarting")) {
			t.Errorf("item %d = %v, want not attempted for the restart", i, it)
		}
	}
	for _, d := range all {
		if m, _ := d["message"].(string); strings.Contains(m, "context canceled") {
			t.Errorf("a decision carries the raw context error: %v", d)
		}
	}
	if strings.Contains(out, "context canceled") {
		t.Errorf("the model read the raw context error: %q", out)
	}
}

// deadCtxSnapshots is a snapshot store that, like a database driver, refuses a call on an ended context.
type deadCtxSnapshots struct{ *testutil.FakeBackend }

func (b deadCtxSnapshots) PushFlowSnapshot(ctx context.Context, s *storageif.FlowSnapshot, lim storageif.FlowSnapshotLimits) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.FakeBackend.PushFlowSnapshot(ctx, s, lim)
}

// TestPushSnapshot_LandsAfterTheCallerEnded: the undo entry of a write that committed is stored even when the
// caller's context ended right after the write (a stream stopped, a request cancelled): it follows the write, not
// the caller.
func TestPushSnapshot_LandsAfterTheCallerEnded(t *testing.T) {
	fake := testutil.NewFakeBackend()
	fake.Flows["cloud-flow"] = &storageif.FlowDocument{ID: "cloud-flow", Name: "Main", OwnerID: "u1", Content: []byte(`{}`)}
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewLocalDocumentProvider(), fake, nil, nil, WithSnapshotStore(deadCtxSnapshots{fake}))
	doc := &models.FlowDocument{ID: "cloud-flow", Name: "Main"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc.pushSnapshot(ctx, doc, svc.snapshotCloudSourceText(doc, "Main source", "before fix"))
	rows, err := fake.ListFlowSnapshots(context.Background(), "cloud-flow")
	if err != nil || len(rows) != 1 {
		t.Fatalf("stored snapshots = %d (%v), want the one pushed after the caller ended", len(rows), err)
	}
}

// TestOwnerDecide_DrainingRefusesARelayedApprove: an approve relayed from another replica to a draining owner is
// refused as restarting, and the origin answers it as such (conclusive, not unreachable).
func TestOwnerDecide_DrainingRefusesARelayedApprove(t *testing.T) {
	svc, _ := newFixTestService(fixTestDoc(), nil)
	svc.draining = drain.New()
	_, ctl := liveStream(svc, "stream-1")
	ctl.ownerID = "u1"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.requestApproval(ctx, "stream-1", approvalRequest{id: "p-relay"}, func(string, map[string]interface{}) {}, nil)
	waitPending(t, svc, "p-relay")
	svc.draining.Begin()

	if got := svc.ownerDecide(context.Background(), chatCommand{Op: opDecide, StreamID: "stream-1", Scope: "u1", ProposalID: "p-relay", Approved: true}); got != replyRestarting {
		t.Fatalf("owner reply = %q, want %q", got, replyRestarting)
	}
	if conclusive, err := decisionResult(replyRestarting, ""); !conclusive || !errors.Is(err, ErrServerRestarting) {
		t.Errorf("decisionResult(restarting) = %v, %v; want conclusive ErrServerRestarting", conclusive, err)
	}
}
