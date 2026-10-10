package api

import (
	"context"
	"strings"
	"sync"
	"testing"

	mailer "pad-analyzer/internal/mail"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// captureSend records the messages handed to the mail service.
type captureSend struct {
	mu   sync.Mutex
	sent []mailer.Message
}

func (c *captureSend) Enabled() bool { return true }
func (c *captureSend) Send(_ context.Context, _ string, msg mailer.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg)
	return nil
}

// assigneeBackend serves the assigned finding, the assignee's user row, and
// (optionally) the assignee as a viewer collaborator of the flow.
type assigneeBackend struct {
	*testutil.FakeBackend
	assigneeIsCollaborator bool
}

func (b *assigneeBackend) LoadUserByID(_ context.Context, id string) (*storageif.User, error) {
	if id == "u-assignee" {
		return &storageif.User{ID: id, Email: "assignee@example.com", DisplayName: "Una"}, nil
	}
	return nil, storageif.ErrNotFound
}

func (b *assigneeBackend) ListCollaborators(_ context.Context, _ string, _ int) ([]*storageif.Collaborator, error) {
	if !b.assigneeIsCollaborator {
		return nil, nil
	}
	return []*storageif.Collaborator{{UserID: "u-assignee", Permission: "viewer"}}, nil
}

// TestFindingEmails_FlowNameGatedOnAssigneeAccess: the flow name in the
// comment and assignment emails arrives resolved through the actor's access;
// it is only disclosed to an assignee who can read the flow themselves (an
// editor may assign a user who is not a collaborator). The assignment email
// used to name the flow to anyone it was assigned to.
func TestFindingEmails_FlowNameGatedOnAssigneeAccess(t *testing.T) {
	for _, tc := range []struct {
		name         string
		collaborator bool
		wantName     bool
	}{
		{"assignee cannot read the flow", false, false},
		{"assignee is a viewer", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &assigneeBackend{FakeBackend: testutil.NewFakeBackend(), assigneeIsCollaborator: tc.collaborator}
			backend.Flows["flow-c"] = &storageif.FlowDocument{ID: "flow-c", Name: "Invoices", OwnerID: "alice", Content: []byte(`{}`)}
			if err := backend.SetFindingStatus(context.Background(), &storageif.FindingStatus{
				FlowID: "flow-c", FindingKey: "unused-variable:b1", RuleID: "unused-variable", Status: "open", AssigneeID: "u-assignee", UpdatedBy: "alice",
			}); err != nil {
				t.Fatalf("seed status: %v", err)
			}
			rt := newTestRouter(backend, true)
			h := moduleOf[*AnalysisHandler](t, rt)
			sink := &captureSend{}
			h.email = mailer.NewServiceWithMailer(sink, "")

			h.notifyFindingComment(context.Background(), "flow-c", "unused-variable:b1", "Invoices", "alice", "Alice", "looks risky")
			h.notifyFindingAssignment(context.Background(), "flow-c", "u-assignee", "Alice", "Invoices", "unused-variable")
			if err := h.email.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}

			if len(sink.sent) != 2 {
				t.Fatalf("emails sent = %d, want 2", len(sink.sent))
			}
			for _, msg := range sink.sent {
				if got := strings.Contains(msg.Text, "Invoices") || strings.Contains(msg.HTML, "Invoices"); got != tc.wantName {
					t.Errorf("%s names the flow = %v, want %v:\n%s", msg.Template, got, tc.wantName, msg.Text)
				}
			}
		})
	}
}
