package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// memberBackend overrides the FakeBackend's org listing with a fixed
// membership set (org "org-A" has user "user-1"; nobody else).
type memberBackend struct {
	testutil.FakeBackend
	listErr error
}

func (m *memberBackend) ListOrgsForUser(_ context.Context, userID string) ([]*interfaces.Organisation, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	if userID == "user-1" {
		return []*interfaces.Organisation{{ID: "org-A"}}, nil
	}
	return nil, nil
}

// recordingKeyFactory counts embedder resolutions so tests can prove whether
// the RAG search path was even reached (Search resolves its embedding provider
// through the factory before anything else).
func recordingKeyFactory(keys map[string]bool, calls *int) *ai.ProviderFactory {
	return ai.NewProviderFactory(func(_ string, provider string) (string, error) {
		*calls++
		if keys[provider] {
			return "test-key", nil
		}
		return "", nil
	}, nil, nil, nil)
}

// TestIsOrgMember pins the S2 gate's own semantics: membership per the org
// store, fail-closed on store errors, and no open access in local mode.
func TestIsOrgMember(t *testing.T) {
	svc := &ChatService{backend: &memberBackend{}}

	if !svc.isOrgMember(context.Background(), "org-A", "user-1") {
		t.Error("member user should pass")
	}
	if svc.isOrgMember(context.Background(), "org-A", "user-2") {
		t.Error("non-member user should fail")
	}
	if svc.isOrgMember(context.Background(), "org-B", "user-1") {
		t.Error("member of a DIFFERENT org should fail")
	}
	if svc.isOrgMember(context.Background(), "", "user-1") {
		t.Error("empty orgID should fail closed")
	}
	if svc.isOrgMember(context.Background(), "org-A", "") {
		t.Error("empty userID (local mode) should fail closed")
	}

	failing := &ChatService{backend: &memberBackend{listErr: errors.New("store down")}}
	if failing.isOrgMember(context.Background(), "org-A", "user-1") {
		t.Error("store error should fail closed, not open the gate")
	}

	local := &ChatService{backend: nil}
	if local.isOrgMember(context.Background(), "org-A", "user-1") {
		t.Error("nil backend (local mode) should fail closed")
	}
}

// TestRagGuidelines_MembershipGated proves the ordering: a NON-member's turn
// never reaches the knowledge search (0 embedder resolutions), a member's
// turn does (search runs and returns empty over the nil store). Before S2 a
// flow collaborator without org membership got the org's guidelines injected.
func TestRagGuidelines_MembershipGated(t *testing.T) {
	orgDoc := &models.FlowDocument{ID: "f1", OrganizationID: "org-A"}

	newSvc := func(calls *int) *ChatService {
		return &ChatService{
			backend:   &memberBackend{},
			knowledge: rag.NewKnowledgeService(nil, recordingKeyFactory(map[string]bool{"openai": true}, calls), nil),
		}
	}

	t.Run("non-member never searches", func(t *testing.T) {
		calls := 0
		svc := newSvc(&calls)
		if got := svc.ragGuidelines(context.Background(), "user-2", orgDoc, "question"); got != "" {
			t.Errorf("guidelines = %q, want empty for non-member", got)
		}
		if calls != 0 {
			t.Errorf("embedder resolved %d times for a non-member — search ran past the gate", calls)
		}
	})

	t.Run("member reaches the search", func(t *testing.T) {
		calls := 0
		svc := newSvc(&calls)
		if got := svc.ragGuidelines(context.Background(), "user-1", orgDoc, "question"); got != "" {
			t.Errorf("guidelines = %q, want empty (nil store returns no hits)", got)
		}
		if calls == 0 {
			t.Error("embedder never resolved for a member — gate is too strict")
		}
	})
}
