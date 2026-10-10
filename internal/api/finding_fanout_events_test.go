package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// fanOutBackend is a FakeBackend with readable users (the email recipients) and a stored comment thread.
type fanOutBackend struct {
	*testutil.FakeBackend
	mu       sync.Mutex
	users    map[string]*storageif.User
	comments []*storageif.FindingComment
}

func newFanOutBackend() *fanOutBackend {
	b := &fanOutBackend{FakeBackend: testutil.NewFakeBackend(), users: map[string]*storageif.User{
		"alice": {ID: "alice", DisplayName: "Alice", Email: "alice@example.com"},
		"bob":   {ID: "bob", DisplayName: "Bob", Email: "bob@example.com"},
		"carol": {ID: "carol", DisplayName: "Carol", Email: "carol@example.com"},
	}}
	b.Flows["flow-f"] = &storageif.FlowDocument{ID: "flow-f", Name: "Invoices", OwnerID: "alice", Content: []byte(`{}`)}
	return b
}

func (b *fanOutBackend) LoadUserByID(_ context.Context, id string) (*storageif.User, error) {
	if u, ok := b.users[id]; ok {
		return u, nil
	}
	return nil, storageif.ErrNotFound
}

func (b *fanOutBackend) AddFindingComment(_ context.Context, c *storageif.FindingComment) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c.ID = "c-" + string(rune('0'+len(b.comments)))
	cp := *c
	b.comments = append(b.comments, &cp)
	return nil
}

func (b *fanOutBackend) ListFindingComments(_ context.Context, flowID, key string) ([]*storageif.FindingComment, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*storageif.FindingComment
	for _, c := range b.comments {
		if c.FlowID == flowID && c.FindingKey == key {
			out = append(out, c)
		}
	}
	return out, nil
}

func (b *fanOutBackend) alertTargets(typ string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, a := range b.GovernanceAlerts {
		if a.Type == typ {
			out = append(out, a.TargetUser)
		}
	}
	sort.Strings(out)
	return out
}

func (b *fanOutBackend) alertCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.GovernanceAlerts)
}

// RecordGovernanceAlert serialises the fake's map writes against alertTargets.
func (b *fanOutBackend) RecordGovernanceAlert(ctx context.Context, a *storageif.GovernanceAlert) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.FakeBackend.RecordGovernanceAlert(ctx, a)
}

type sentMail struct{ to, subject, text string }

type captureMailer struct {
	mu   sync.Mutex
	sent []sentMail
}

func (m *captureMailer) Send(_ context.Context, to string, msg mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{to, msg.Subject, msg.Text})
	return nil
}

func (m *captureMailer) Enabled() bool { return true }

func (m *captureMailer) all() []sentMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sentMail(nil), m.sent...)
}

func fanOutRouter(t *testing.T) (*Router, *fanOutBackend, *captureMailer, *AnalysisHandler) {
	t.Helper()
	backend := newFanOutBackend()
	rt := newTestRouter(backend, true)
	mails := &captureMailer{}
	h := moduleOf[*AnalysisHandler](t, rt)
	h.email = mailer.NewServiceWithMailer(mails, "")
	return rt, backend, mails, h
}

// The fan-outs now follow FindingAssigned and FindingCommented; who gets an alert row and an email is what it
// was: the new assignee (alert + email), then on a comment the assignee and earlier commenters (alerts, the actor
// never) and the assignee's email quoting the comment.
func TestFindingFanOut_RecipientsAlertsAndEmails(t *testing.T) {
	rt, backend, mails, _ := fanOutRouter(t)
	alice := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/set", alice, map[string]any{
		"flowId": "flow-f", "findingKey": "unused-variable:b1", "ruleId": "unused-variable", "status": "acknowledged", "assigneeId": "bob",
	})
	checkStatus(t, rr, http.StatusOK)
	waitUntil(t, "the assignment email", func() bool { return len(mails.all()) == 1 })
	if got := backend.alertTargets("finding_assigned"); len(got) != 1 || got[0] != "bob" {
		t.Fatalf("assignment alerts for %v, want bob", got)
	}
	if m := mails.all()[0]; m.to != "bob@example.com" || m.subject != "A finding was assigned to you" || !strings.Contains(m.text, "Alice assigned") {
		t.Fatalf("assignment email = %+v", m)
	}

	// The same assignee again with a status change: no new alert, no email.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/set", alice, map[string]any{
		"flowId": "flow-f", "findingKey": "unused-variable:b1", "ruleId": "unused-variable", "status": "in_progress", "assigneeId": "bob",
	})
	checkStatus(t, rr, http.StatusOK)

	backend.mu.Lock()
	backend.comments = append(backend.comments, &storageif.FindingComment{ID: "c-old", FlowID: "flow-f", FindingKey: "unused-variable:b1", AuthorID: "carol"})
	backend.mu.Unlock()
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/comments/add", alice, map[string]any{
		"flowId": "flow-f", "findingKey": "unused-variable:b1", "body": "please look",
	})
	checkStatus(t, rr, http.StatusOK)
	waitUntil(t, "the comment alerts and email", func() bool {
		return len(backend.alertTargets("comment_added")) == 2 && len(mails.all()) == 2
	})
	if got := backend.alertTargets("comment_added"); got[0] != "bob" || got[1] != "carol" {
		t.Fatalf("comment alerts for %v, want bob and carol (never the actor)", got)
	}
	m := mails.all()[1]
	if m.to != "bob@example.com" || m.subject != "New comment on your assigned finding" || !strings.Contains(m.text, "please look") {
		t.Fatalf("comment email = %+v", m)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(mails.all()); n != 2 {
		t.Fatalf("%d emails, want 2 (the repeated assignee is not re-notified)", n)
	}
	if n := backend.alertCount(); n != 3 {
		t.Fatalf("%d alert rows, want 3", n)
	}
}

// A rolled-back comment or assignment alerts and emails no one; a committed one does, only after the commit.
func TestFindingFanOut_OnlyAfterCommit(t *testing.T) {
	_, backend, mails, h := fanOutRouter(t)
	flow := service.FlowRef{ID: "flow-f", Name: "Invoices", OwnerID: "alice"}

	ctx, reg := txhook.With(context.Background())
	if err := h.triage.SetStatus(ctx, flow, &storageif.FindingStatus{FlowID: "flow-f", FindingKey: "k1", RuleID: "r", Status: "open", AssigneeID: "bob", UpdatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := h.comments.Add(ctx, flow, &storageif.FindingComment{FlowID: "flow-f", FindingKey: "k1", AuthorID: "alice", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	reg.Abort()
	time.Sleep(100 * time.Millisecond)
	if n, m := backend.alertCount(), len(mails.all()); n != 0 || m != 0 {
		t.Fatalf("rolled back: %d alert rows and %d emails, want none", n, m)
	}

	ctx, reg = txhook.With(context.Background())
	if err := h.triage.SetStatus(ctx, flow, &storageif.FindingStatus{FlowID: "flow-f", FindingKey: "k2", RuleID: "r", Status: "open", AssigneeID: "bob", UpdatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := backend.alertCount(); n != 0 {
		t.Fatalf("%d alert rows before the commit", n)
	}
	reg.Commit(nil)
	waitUntil(t, "the committed assignment's alert and email", func() bool {
		return backend.alertCount() == 1 && len(mails.all()) == 1
	})
}
