package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// triageBackend adds the users the notification lookups resolve; getterBackend counts its one-row status reads.
type triageBackend struct {
	*testutil.FakeBackend
	users      map[string]*storageif.User
	getterHits int
}

func (b *triageBackend) LoadUserByID(_ context.Context, id string) (*storageif.User, error) {
	if u, ok := b.users[id]; ok {
		return u, nil
	}
	return nil, storageif.ErrNotFound
}

type getterBackend struct{ *triageBackend }

func (b getterBackend) GetFindingStatus(_ context.Context, flowID, findingKey string) (*storageif.FindingStatus, error) {
	b.getterHits++
	return b.FindingStatuses[flowID][findingKey], nil
}

func newTriageBackend(t *testing.T) *triageBackend {
	t.Helper()
	b := &triageBackend{FakeBackend: testutil.NewFakeBackend(), users: map[string]*storageif.User{
		"u-named":   {ID: "u-named", DisplayName: "Una", Email: "una@example.com"},
		"u-email":   {ID: "u-email", Email: "eve@example.com"},
		"u-noemail": {ID: "u-noemail", DisplayName: "Nobody"},
	}}
	for _, st := range []*storageif.FindingStatus{
		{FlowID: "f1", FindingKey: "rule-a:b1", Status: "open", AssigneeID: "u-named"},
		{FlowID: "f1", FindingKey: "rule-a:b2", Status: "resolved"},
	} {
		if err := b.SetFindingStatus(context.Background(), st); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return b
}

func TestTriageService_Assignee(t *testing.T) {
	ctx := context.Background()
	b := newTriageBackend(t)
	svc := NewTriageService(getterBackend{b}, b, b, nil)
	if got := svc.Assignee(ctx, "f1", "rule-a:b1"); got != "u-named" {
		t.Errorf("assigned finding = %q, want u-named", got)
	}
	if got := svc.Assignee(ctx, "f1", "rule-a:b2"); got != "" {
		t.Errorf("unassigned finding = %q, want empty", got)
	}
	if got := svc.Assignee(ctx, "f1", "rule-a:missing"); got != "" {
		t.Errorf("finding without state = %q, want empty", got)
	}
	if b.getterHits != 3 {
		t.Errorf("point lookups = %d, want 3 (one row per question)", b.getterHits)
	}
}

func TestTriageService_StatusesAndBaseline(t *testing.T) {
	ctx := context.Background()
	b := newTriageBackend(t)
	bus := eventbus.New(nil, time.Second)
	rec := eventbus.NewRecorder(bus)
	svc := NewTriageService(b, b, b, bus)
	f1 := FlowRef{ID: "f1", Name: "Payroll", OwnerID: "alice", OrgID: "org-1"}

	if err := svc.SetStatusBatch(ctx, f1, "u-named", []*storageif.FindingStatus{{FindingKey: "rule-a:b3", Status: "suppressed"}}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if err := svc.ClearStatus(ctx, f1, "rule-a:b2"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	list, err := svc.ListStatuses(ctx, "f1")
	if err != nil || len(list) != 2 || list[0].FindingKey != "rule-a:b1" || list[1].FindingKey != "rule-a:b3" {
		t.Fatalf("statuses = %+v, %v; want b1 and b3", list, err)
	}

	if bl, err := svc.Baseline(ctx, "f1"); err != nil || bl != nil {
		t.Fatalf("baseline before set = %+v, %v; want nil", bl, err)
	}
	if err := svc.SetBaseline(ctx, f1, &storageif.FlowBaseline{FlowID: "f1", Keys: []string{"k"}}); err != nil {
		t.Fatalf("set baseline: %v", err)
	}
	if bl, _ := svc.Baseline(ctx, "f1"); bl == nil || len(bl.Keys) != 1 {
		t.Fatalf("baseline = %+v, want one key", bl)
	}
	if err := svc.ClearBaseline(ctx, f1); err != nil {
		t.Fatalf("clear baseline: %v", err)
	}
	if bl, _ := svc.Baseline(ctx, "f1"); bl != nil {
		t.Fatalf("baseline after clear = %+v, want nil", bl)
	}

	// Every write announced the flow's state change with its owner and organisation; a batch and the baseline
	// name no single finding.
	var keys []string
	for _, e := range eventbus.Recorded[events.FindingStatusChanged](rec) {
		if e.FlowID != "f1" || e.OwnerID != "alice" || e.OrgID != "org-1" {
			t.Errorf("event %+v does not name f1, its owner and organisation", e)
		}
		keys = append(keys, e.FindingKey)
	}
	if want := []string{"", "rule-a:b2", "", ""}; !slices.Equal(keys, want) {
		t.Errorf("finding keys announced %q, want %q", keys, want)
	}
	if n := len(rec.Events()); n != 4 {
		t.Errorf("%d events published, want 4 (a batch assigns no one)", n)
	}
}

type failingStatusStore struct{ *triageBackend }

func (failingStatusStore) SetFindingStatus(context.Context, *storageif.FindingStatus) error {
	return errors.New("boom")
}

// SetStatus announces the status change, and an assignment only when the assignee is new: the previous assignee,
// re-sent with a status change, notifies no one. A failed write announces nothing.
func TestTriageService_SetStatusAnnouncesNewAssignee(t *testing.T) {
	ctx := context.Background()
	b := newTriageBackend(t)
	bus := eventbus.New(nil, time.Second)
	rec := eventbus.NewRecorder(bus)
	svc := NewTriageService(b, b, b, bus)
	f1 := FlowRef{ID: "f1", Name: "Payroll", OwnerID: "alice", OrgID: "org-1"}

	// rule-a:b1 is already assigned to u-named.
	if err := svc.SetStatus(ctx, f1, &storageif.FindingStatus{FlowID: "f1", FindingKey: "rule-a:b1", RuleID: "rule-a", Status: "in_progress", AssigneeID: "u-named", UpdatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	if got := eventbus.Recorded[events.FindingAssigned](rec); len(got) != 0 {
		t.Fatalf("re-sending the same assignee announced %+v", got)
	}
	if err := svc.SetStatus(ctx, f1, &storageif.FindingStatus{FlowID: "f1", FindingKey: "rule-a:b1", RuleID: "rule-a", Status: "in_progress", AssigneeID: "u-email", UpdatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	want := events.FindingAssigned{FlowID: "f1", FlowName: "Payroll", OwnerID: "alice", OrgID: "org-1", FindingKey: "rule-a:b1", RuleID: "rule-a", AssigneeID: "u-email", AssignerID: "alice"}
	if got := eventbus.Recorded[events.FindingAssigned](rec); len(got) != 1 || got[0] != want {
		t.Fatalf("assignment announced %+v, want %+v", got, want)
	}
	if got := eventbus.Recorded[events.FindingStatusChanged](rec); len(got) != 2 || got[1].FindingKey != "rule-a:b1" {
		t.Fatalf("status changes announced %+v, want two for rule-a:b1", got)
	}

	rec.Reset()
	failing := NewTriageService(failingStatusStore{b}, b, b, bus)
	if err := failing.SetStatus(ctx, f1, &storageif.FindingStatus{FlowID: "f1", FindingKey: "rule-a:b9", AssigneeID: "u-named", UpdatedBy: "alice"}); err == nil {
		t.Fatal("store error swallowed")
	}
	if got := rec.Events(); len(got) != 0 {
		t.Fatalf("a failed write announced %+v", got)
	}
}

func TestTriageService_RecordAlertStampsCreatedAt(t *testing.T) {
	b := newTriageBackend(t)
	svc := NewTriageService(b, b, b, nil)
	a := &storageif.GovernanceAlert{ID: "f1|finding_assigned|k|u", Type: "finding_assigned", TargetUser: "u"}
	if err := svc.RecordAlert(context.Background(), a); err != nil {
		t.Fatalf("record: %v", err)
	}
	if a.CreatedAt.IsZero() || a.CreatedAt.Location().String() != "UTC" {
		t.Errorf("CreatedAt = %v, want a UTC timestamp", a.CreatedAt)
	}
	if b.GovernanceAlerts[a.ID] == nil {
		t.Error("alert not persisted")
	}
}

func TestTriageService_DisplayNameAndRecipient(t *testing.T) {
	ctx := context.Background()
	b := newTriageBackend(t)
	svc := NewTriageService(b, b, b, nil)

	for _, tc := range []struct{ userID, fallback, want string }{
		{"u-named", "claims@example.com", "Una"},
		{"u-email", "claims@example.com", "eve@example.com"},
		{"u-gone", "claims@example.com", "claims@example.com"},
		{"", "claims@example.com", "claims@example.com"},
		{"u-gone", "", "a teammate"},
	} {
		if got := svc.DisplayName(ctx, tc.userID, tc.fallback); got != tc.want {
			t.Errorf("DisplayName(%q, %q) = %q, want %q", tc.userID, tc.fallback, got, tc.want)
		}
	}

	if u := svc.Recipient(ctx, "u-named"); u == nil || u.Email != "una@example.com" {
		t.Errorf("Recipient(u-named) = %+v, want Una", u)
	}
	if u := svc.Recipient(ctx, "u-noemail"); u != nil {
		t.Errorf("Recipient without an email = %+v, want nil", u)
	}
	if u := svc.Recipient(ctx, "u-gone"); u != nil {
		t.Errorf("Recipient of an unknown user = %+v, want nil", u)
	}
}

func TestTriageService_StoreErrorsPassThrough(t *testing.T) {
	b := newTriageBackend(t)
	want := errors.New("boom")
	b.BatchSetFindingStatusErr = want
	svc := NewTriageService(b, b, b, nil)
	if err := svc.SetStatusBatch(context.Background(), FlowRef{ID: "f1"}, "u", []*storageif.FindingStatus{{FindingKey: "k", Status: "open"}}); !errors.Is(err, want) {
		t.Errorf("batch error = %v, want %v", err, want)
	}
}
