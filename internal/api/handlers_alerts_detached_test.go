package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// gatedAlertBackend is a FakeBackend whose alert write blocks until the test
// releases it (or one second passes, which is recorded): a response that
// completes while the write is still gated was not waiting on it. It also
// counts user lookups per id, fails the content read (LoadFlow) and serves
// seeded prior comments.
type gatedAlertBackend struct {
	*testutil.FakeBackend
	release  chan struct{}
	alerts   atomic.Int64
	timedOut atomic.Int64
	loadFlow atomic.Int64

	mu          sync.Mutex
	userLookups map[string]int
	prior       []*storageif.FindingComment
}

func newGatedAlertBackend() *gatedAlertBackend {
	return &gatedAlertBackend{FakeBackend: testutil.NewFakeBackend(), release: make(chan struct{}), userLookups: map[string]int{}}
}

func (b *gatedAlertBackend) LoadUserByID(_ context.Context, id string) (*storageif.User, error) {
	b.mu.Lock()
	b.userLookups[id]++
	b.mu.Unlock()
	return nil, storageif.ErrNotFound // the claims email is the fallback
}

func (b *gatedAlertBackend) lookups(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.userLookups[id]
}

func (b *gatedAlertBackend) LoadFlow(context.Context, string) (*storageif.FlowDocument, error) {
	b.loadFlow.Add(1)
	return nil, errors.New("content read must not happen")
}

func (b *gatedAlertBackend) ListFindingComments(context.Context, string, string) ([]*storageif.FindingComment, error) {
	return b.prior, nil
}

func (b *gatedAlertBackend) RecordGovernanceAlert(ctx context.Context, a *storageif.GovernanceAlert) error {
	select {
	case <-b.release:
	case <-time.After(time.Second):
		b.timedOut.Add(1)
	}
	err := b.FakeBackend.RecordGovernanceAlert(ctx, a)
	b.alerts.Add(1)
	return err
}

// alertsByTarget returns the recorded alerts keyed by target user. Call only
// after the expected number of alerts has been counted (happens-before via
// the atomic).
func (b *gatedAlertBackend) alertsByTarget() map[string]*storageif.GovernanceAlert {
	out := map[string]*storageif.GovernanceAlert{}
	for _, a := range b.GovernanceAlerts {
		out[a.TargetUser] = a
	}
	return out
}

// TestAddComment_FanOutDetachedAndHeaderOnly: adding a comment with three
// alert targets (the assignee and two prior commenters) answers before any
// alert is written, resolves the actor's name once (not per target; the
// claims email stands in when the row is unreadable), and never reads the
// flow's content for its name.
func TestAddComment_FanOutDetachedAndHeaderOnly(t *testing.T) {
	backend := newGatedAlertBackend()
	backend.Flows["flow-c"] = &storageif.FlowDocument{ID: "flow-c", Name: "Invoices", OwnerID: "alice", Content: []byte(`{}`)}
	if err := backend.SetFindingStatus(context.Background(), &storageif.FindingStatus{
		FlowID: "flow-c", FindingKey: "unused-variable:b1", RuleID: "unused-variable", Status: "open", AssigneeID: "u-assignee", UpdatedBy: "alice",
	}); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	backend.prior = []*storageif.FindingComment{
		{ID: "c-1", FlowID: "flow-c", FindingKey: "unused-variable:b1", AuthorID: "u-prev1", Body: "first"},
		{ID: "c-2", FlowID: "flow-c", FindingKey: "unused-variable:b1", AuthorID: "u-prev2", Body: "second"},
	}
	rt := newTestRouter(backend, true)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	started := time.Now()
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/add", bearer, map[string]any{
		"flowId": "flow-c", "findingKey": "unused-variable:b1", "body": "looks fine",
	})
	checkStatus(t, rr, http.StatusOK)
	if took := time.Since(started); took > 800*time.Millisecond {
		t.Fatalf("response took %v: it waited on the gated alert write", took)
	}
	if n := backend.alerts.Load(); n != 0 {
		t.Fatalf("%d alerts written before the response was sent", n)
	}

	close(backend.release)
	waitUntil(t, "three comment alerts", func() bool { return backend.alerts.Load() == 3 })
	if n := backend.timedOut.Load(); n != 0 {
		t.Fatalf("%d alert writes timed out waiting for release: the fan-out ran inline", n)
	}
	if n := backend.lookups("alice"); n != 1 {
		t.Fatalf("actor looked up %d times for 3 targets; want exactly 1", n)
	}
	if n := backend.loadFlow.Load(); n != 0 {
		t.Fatalf("LoadFlow (content) called %d times; the name comes from the header", n)
	}
	byTarget := backend.alertsByTarget()
	for _, target := range []string{"u-assignee", "u-prev1", "u-prev2"} {
		a := byTarget[target]
		if a == nil {
			t.Fatalf("no alert for %s: %v", target, byTarget)
		}
		if a.Type != "comment_added" || a.FlowName != "Invoices" || a.Message != "New comment from alice@example.com" {
			t.Fatalf("alert for %s = type:%q flow:%q message:%q", target, a.Type, a.FlowName, a.Message)
		}
	}
	if _, self := byTarget["alice"]; self {
		t.Fatal("the actor was alerted about their own comment")
	}
}

// TestSetFindingStatus_AssignmentAlertDetached: assigning a finding answers
// before the assignee's alert is written; the alert names the assigner from
// the claims when the user row is unreadable, without a lookup per target.
func TestSetFindingStatus_AssignmentAlertDetached(t *testing.T) {
	backend := newGatedAlertBackend()
	backend.Flows["flow-t"] = &storageif.FlowDocument{ID: "flow-t", Name: "Payroll", OwnerID: "alice", Content: []byte(`{}`)}
	rt := newTestRouter(backend, true)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	started := time.Now()
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/set", bearer, map[string]any{
		"flowId": "flow-t", "findingKey": "unused-variable:b1", "ruleId": "unused-variable", "status": "acknowledged", "assigneeId": "u-bob",
	})
	checkStatus(t, rr, http.StatusOK)
	if took := time.Since(started); took > 800*time.Millisecond {
		t.Fatalf("response took %v: it waited on the gated alert write", took)
	}
	if n := backend.alerts.Load(); n != 0 {
		t.Fatalf("%d alerts written before the response was sent", n)
	}

	close(backend.release)
	waitUntil(t, "the assignment alert", func() bool { return backend.alerts.Load() == 1 })
	if n := backend.timedOut.Load(); n != 0 {
		t.Fatalf("%d alert writes timed out waiting for release: the hook ran the write inline", n)
	}
	if n := backend.lookups("alice"); n != 1 {
		t.Fatalf("assigner looked up %d times; want exactly 1", n)
	}
	a := backend.alertsByTarget()["u-bob"]
	if a == nil || a.Type != "finding_assigned" || a.FlowName != "Payroll" || a.Message != "Assigned to you by alice@example.com" {
		t.Fatalf("assignment alert = %+v", a)
	}
}
