package contract

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/storage/interfaces"
)

// RunAuditSuite runs the audit-log store contract: Postgres (DB-gated) and the test fake agree on what a saved
// row reads back as, including the organisation and actor columns of v33, and on the list's order and filters.
func RunAuditSuite(t *testing.T, store interfaces.AuditStore) {
	t.Helper()
	for _, sc := range auditScenarios {
		t.Run(sc.name, func(t *testing.T) { sc.run(t, store) })
	}
}

var auditScenarios = []struct {
	name string
	run  func(t *testing.T, store interfaces.AuditStore)
}{
	{"every_column_round_trips", auditRoundTrip},
	{"empty_actor_and_org_read_back_empty", auditEmptyColumns},
	{"list_orders_newest_first_and_filters", auditListOrder},
	{"batch_is_written_all_or_nothing", auditBatch},
}

func auditEvent(userID, action string, at time.Time) *interfaces.AuditEvent {
	return &interfaces.AuditEvent{
		ID: "contract-" + uuid.NewString(), UserID: userID, Email: userID + "@example.com", Action: action,
		ResourceType: "flow", ResourceID: "flow-" + userID, IP: "203.0.113.7",
		Meta: map[string]string{"k": "v"}, CreatedAt: at.UTC().Truncate(time.Microsecond),
	}
}

func listOne(t *testing.T, store interfaces.AuditStore, userID string) *interfaces.AuditEvent {
	t.Helper()
	got, err := store.ListAuditEvents(context.Background(), interfaces.AuditFilter{UserID: userID})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListAuditEvents(%s) = %d rows, want 1", userID, len(got))
	}
	return got[0]
}

func auditRoundTrip(t *testing.T, store interfaces.AuditStore) {
	user := "contract-audit-" + uuid.NewString()[:8]
	want := auditEvent(user, "contract.round_trip", time.Now())
	want.OrgID, want.ActorKind, want.TokenID, want.ActorJob, want.UserAgent = "org-1", "token", "tok-1", "", "Mozilla/5.0 (contract)"
	if err := store.SaveAuditEvent(context.Background(), want); err != nil {
		t.Fatalf("SaveAuditEvent: %v", err)
	}
	got := listOne(t, store, user)
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	got.CreatedAt = want.CreatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read back %+v\nwant %+v", got, want)
	}

	sys := auditEvent("", "contract.system", time.Now())
	sys.ResourceID = "job-" + user
	sys.ActorKind, sys.ActorJob = "system", "retention"
	if err := store.SaveAuditEvent(context.Background(), sys); err != nil {
		t.Fatalf("SaveAuditEvent(system): %v", err)
	}
	rows, err := store.ListAuditEvents(context.Background(), interfaces.AuditFilter{Action: "contract.system", Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == sys.ID {
			if r.ActorKind != "system" || r.ActorJob != "retention" || r.UserID != "" {
				t.Errorf("system row read back as kind %q job %q user %q", r.ActorKind, r.ActorJob, r.UserID)
			}
			return
		}
	}
	t.Error("the system actor's row is not listed")
}

func auditEmptyColumns(t *testing.T, store interfaces.AuditStore) {
	user := "contract-audit-" + uuid.NewString()[:8]
	ev := auditEvent(user, "contract.empty", time.Now())
	if err := store.SaveAuditEvent(context.Background(), ev); err != nil {
		t.Fatalf("SaveAuditEvent: %v", err)
	}
	got := listOne(t, store, user)
	if got.OrgID != "" || got.ActorKind != "" || got.TokenID != "" || got.ActorJob != "" || got.UserAgent != "" {
		t.Errorf("empty columns read back as org %q kind %q token %q job %q agent %q",
			got.OrgID, got.ActorKind, got.TokenID, got.ActorJob, got.UserAgent)
	}
}

func auditListOrder(t *testing.T, store interfaces.AuditStore) {
	user := "contract-audit-" + uuid.NewString()[:8]
	base := time.Now().Add(-time.Hour)
	older := auditEvent(user, "contract.older", base)
	newer := auditEvent(user, "contract.newer", base.Add(time.Minute))
	tiedA := auditEvent(user, "contract.tied", base.Add(2*time.Minute))
	tiedB := auditEvent(user, "contract.tied", base.Add(2*time.Minute))
	tiedA.ID, tiedB.ID = "contract-"+user+"-a", "contract-"+user+"-b"
	subject := auditEvent("", "contract.subject", base.Add(3*time.Minute))
	subject.ResourceType, subject.ResourceID = "user", user
	for _, ev := range []*interfaces.AuditEvent{tiedB, older, subject, newer, tiedA} {
		if err := store.SaveAuditEvent(context.Background(), ev); err != nil {
			t.Fatalf("SaveAuditEvent: %v", err)
		}
	}
	ids := func(f interfaces.AuditFilter) []string {
		t.Helper()
		rows, err := store.ListAuditEvents(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.ID
		}
		return out
	}
	if got, want := ids(interfaces.AuditFilter{UserID: user}), []string{tiedA.ID, tiedB.ID, newer.ID, older.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("by user = %v, want %v (newest first, id breaks ties)", got, want)
	}
	if got, want := ids(interfaces.AuditFilter{UserID: user, IncludeSubject: true, Limit: 2, Offset: 1}), []string{tiedA.ID, tiedB.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("by user with subject, page 2 of size 2 from 1 = %v, want %v", got, want)
	}
	if got, want := ids(interfaces.AuditFilter{UserID: user, Action: "contract.tied"}), []string{tiedA.ID, tiedB.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("by user and action = %v, want %v", got, want)
	}
	since := base.Add(90 * time.Second)
	if got, want := ids(interfaces.AuditFilter{UserID: user, Since: &since}), []string{tiedA.ID, tiedB.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("by user since = %v, want %v", got, want)
	}
}

// auditBatch: SaveAuditEvents writes every row of a batch or none of them. The recorder relies on it: after a
// failed batch it writes the rows one by one, which would duplicate a partly written batch.
func auditBatch(t *testing.T, store interfaces.AuditStore) {
	ctx := context.Background()
	user := "contract-audit-" + uuid.NewString()[:8]
	now := time.Now()
	batch := []*interfaces.AuditEvent{
		auditEvent(user, "contract.batch", now.Add(-2*time.Second)),
		auditEvent(user, "contract.batch", now.Add(-time.Second)),
		auditEvent(user, "contract.batch", now),
	}
	if err := store.SaveAuditEvents(ctx, batch); err != nil {
		t.Fatalf("SaveAuditEvents: %v", err)
	}
	if err := store.SaveAuditEvents(ctx, nil); err != nil {
		t.Errorf("an empty batch: %v", err)
	}
	rows, err := store.ListAuditEvents(ctx, interfaces.AuditFilter{UserID: user})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].ID != batch[2].ID || rows[2].ID != batch[0].ID {
		t.Fatalf("listed %d rows, want the batch's 3 newest first", len(rows))
	}

	// A row whose id is taken fails the batch, and the batch's other row is not written either.
	fresh := auditEvent(user, "contract.batch.rejected", now)
	if err := store.SaveAuditEvents(ctx, []*interfaces.AuditEvent{fresh, auditEvent(user, "contract.batch.rejected", now)}); err != nil {
		t.Fatalf("SaveAuditEvents (distinct ids): %v", err)
	}
	again := auditEvent(user, "contract.batch.rejected", now)
	dup := *batch[0]
	if err := store.SaveAuditEvents(ctx, []*interfaces.AuditEvent{again, &dup}); err == nil {
		t.Fatal("a batch with a duplicate id was accepted")
	}
	rows, err = store.ListAuditEvents(ctx, interfaces.AuditFilter{UserID: user, Action: "contract.batch.rejected"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == again.ID {
			t.Error("a row of the failed batch was written")
		}
	}
	if len(rows) != 2 {
		t.Errorf("listed %d rows of the accepted batch, want 2", len(rows))
	}
}
