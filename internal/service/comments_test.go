package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// commentBackend serves a fixed thread (FakeBackend's comment methods are no-ops).
type commentBackend struct {
	*triageBackend
	thread []*storageif.FindingComment
}

func (b *commentBackend) ListFindingComments(_ context.Context, _, _ string) ([]*storageif.FindingComment, error) {
	return b.thread, nil
}

func TestCommentService_AlertTargets(t *testing.T) {
	b := &commentBackend{triageBackend: newTriageBackend(t), thread: []*storageif.FindingComment{
		{ID: "c1", AuthorID: "u-prev"},
		{ID: "c2", AuthorID: "u-named"}, // the assignee commented too: one target
		{ID: "c3", AuthorID: ""},
		{ID: "c-new", AuthorID: "u-actor"}, // the comment being announced
	}}
	svc := NewCommentService(b, NewTriageService(b, b, b, nil), nil)

	got := svc.AlertTargets(context.Background(), "f1", "rule-a:b1", "c-new")
	want := map[string]bool{"u-named": true, "u-prev": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}

	if got := svc.AlertTargets(context.Background(), "f1", "rule-a:b2", "c1"); !reflect.DeepEqual(got, map[string]bool{"u-named": true, "u-actor": true}) {
		t.Errorf("unassigned finding targets = %v, want the other commenters only", got)
	}
}

func TestCommentService_AssigneeToNotify(t *testing.T) {
	ctx := context.Background()
	b := newTriageBackend(t)
	svc := NewCommentService(b, NewTriageService(b, b, b, nil), nil)

	if u := svc.AssigneeToNotify(ctx, "f1", "rule-a:b1", "u-other"); u == nil || u.ID != "u-named" {
		t.Errorf("assignee = %+v, want u-named", u)
	}
	if u := svc.AssigneeToNotify(ctx, "f1", "rule-a:b1", "u-named"); u != nil {
		t.Errorf("assignee commenting on their own finding = %+v, want nil", u)
	}
	if u := svc.AssigneeToNotify(ctx, "f1", "rule-a:b2", "u-other"); u != nil {
		t.Errorf("unassigned finding = %+v, want nil", u)
	}
	if got := svc.AuthorName(ctx, "u-email", "x@example.com"); got != "eve@example.com" {
		t.Errorf("AuthorName = %q, want the stored email", got)
	}
}

// idCommentStore assigns the stored comment its ID, as the real stores do, or fails.
type idCommentStore struct {
	*commentBackend
	err error
}

func (s idCommentStore) AddFindingComment(_ context.Context, c *storageif.FindingComment) error {
	if s.err != nil {
		return s.err
	}
	c.ID = "c-9"
	return nil
}

// Add announces the stored comment, with the id the store gave it and the flow the caller authorized; a failed
// write announces nothing.
func TestCommentService_AddAnnouncesTheComment(t *testing.T) {
	b := &commentBackend{triageBackend: newTriageBackend(t)}
	bus := eventbus.New(nil, time.Second)
	rec := eventbus.NewRecorder(bus)
	flow := FlowRef{ID: "f1", Name: "Payroll", OwnerID: "alice", OrgID: "org-1"}
	svc := NewCommentService(idCommentStore{commentBackend: b}, NewTriageService(b, b, b, nil), bus)

	if err := svc.Add(context.Background(), flow, &storageif.FindingComment{FlowID: "f1", FindingKey: "rule-a:b1", AuthorID: "u-actor", Body: "looks fine"}); err != nil {
		t.Fatal(err)
	}
	want := events.FindingCommented{FlowID: "f1", FlowName: "Payroll", OwnerID: "alice", OrgID: "org-1", FindingKey: "rule-a:b1", CommentID: "c-9", AuthorID: "u-actor", Body: "looks fine"}
	if got := eventbus.Recorded[events.FindingCommented](rec); len(got) != 1 || got[0] != want {
		t.Fatalf("announced %+v, want %+v", got, want)
	}

	rec.Reset()
	failing := NewCommentService(idCommentStore{commentBackend: b, err: errors.New("boom")}, NewTriageService(b, b, b, nil), bus)
	if err := failing.Add(context.Background(), flow, &storageif.FindingComment{FlowID: "f1", FindingKey: "k", AuthorID: "u", Body: "x"}); err == nil {
		t.Fatal("store error swallowed")
	}
	if got := rec.Events(); len(got) != 0 {
		t.Fatalf("a failed write announced %+v", got)
	}
}
