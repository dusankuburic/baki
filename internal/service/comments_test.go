package service

import (
	"context"
	"reflect"
	"testing"

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
	svc := NewCommentService(b, NewTriageService(b, b, b))

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
	svc := NewCommentService(b, NewTriageService(b, b, b))

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
