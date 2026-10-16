package database

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// TestKnowledge_UnknownOrganisationIsErrOrgNotFound: a document written without a row-level-security user (local mode
// without accounts) for an organisation that does not exist meets the foreign key of migration 0048, which the store
// answers ErrOrgNotFound (a 404) rather than the bare constraint violation (a 500).
func TestKnowledge_UnknownOrganisationIsErrOrgNotFound(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if err := b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: "kb-gone-1", OrgID: "no-such-org", Filename: "a.md"}); !errors.Is(err, interfaces.ErrOrgNotFound) {
		t.Errorf("SaveKnowledgeDocument: %v, want ErrOrgNotFound", err)
	}
	if err := b.SaveKnowledgeDocumentWithChunks(ctx, "", &interfaces.KnowledgeDocument{ID: "kb-gone-2", OrgID: "no-such-org", Filename: "b.md"}, nil); !errors.Is(err, interfaces.ErrOrgNotFound) {
		t.Errorf("SaveKnowledgeDocumentWithChunks: %v, want ErrOrgNotFound", err)
	}
}
