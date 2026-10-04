package api

import (
	"context"
	"strings"
	"testing"

	mailer "pad-analyzer/internal/mail"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// captureSend records the last message handed to the mail service.
type captureSend struct {
	calls int
	text  string
}

func (c *captureSend) Enabled() bool { return true }
func (c *captureSend) Send(_ context.Context, _, _, text, _ string) error {
	c.calls++
	c.text = text
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

// TestNotifyFindingComment_FlowNameGatedOnAssigneeAccess: the flow name in
// the comment email arrives resolved through the commenter's access; it is
// only disclosed to an assignee who can read the flow themselves (an editor
// may assign a user who is not a collaborator), as the pre-detached code did
// by resolving the document as the assignee.
func TestNotifyFindingComment_FlowNameGatedOnAssigneeAccess(t *testing.T) {
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
			h := rt.handlers.Analysis
			sink := &captureSend{}
			h.email = mailer.NewServiceWithMailer(sink, "")

			h.notifyFindingComment(context.Background(), "flow-c", "unused-variable:b1", "Invoices", "alice", "Alice", "looks risky")

			if sink.calls != 1 {
				t.Fatalf("emails sent = %d, want 1", sink.calls)
			}
			if got := strings.Contains(sink.text, "Invoices"); got != tc.wantName {
				t.Errorf("email names the flow = %v, want %v:\n%s", got, tc.wantName, sink.text)
			}
		})
	}
}
