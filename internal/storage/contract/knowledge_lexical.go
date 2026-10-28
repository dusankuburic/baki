package contract

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// runKnowledgeLexicalScenarios covers the lexical half of hybrid retrieval (R-knowledge-8): a chunk whose text holds a
// phrase of the question at a trigram word similarity at or above the floor is a hit, whatever its embedding; the hits
// keep the org and the identity rule of the vector search (the caller's identity and the legacy rows; "" any); the
// chunk that matches more of the phrases ranks first; no phrases, no hits.
func runKnowledgeLexicalScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	ids := func(chunks []interfaces.KnowledgeChunk) string {
		out := make([]string, len(chunks))
		for i, c := range chunks {
			out[i] = c.ID
		}
		return strings.Join(out, ",")
	}

	run("Knowledge_lexical_search_keeps_the_org_and_identity_rule", func(t *testing.T) {
		admin := "contract-kblex-admin-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: admin, Email: admin + "@example.com", Password: "x", Role: auth.RoleMember}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), admin) })
		orgID, otherOrg := "contract-kblex-org-"+runID, "contract-kblex-other-"+runID
		for _, o := range []string{orgID, otherOrg} {
			if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: o, Name: o, OwnerID: admin, Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}}); err != nil {
				t.Fatalf("SaveOrg(%s): %v", o, err)
			}
			t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), o) })
		}
		docID, otherDoc := "contract-kblex-doc-"+runID, "contract-kblex-otherdoc-"+runID
		if err := b.SaveKnowledgeDocument(ctx, admin, &interfaces.KnowledgeDocument{ID: docID, OrgID: orgID, Filename: "lex.md", ChunkerVersion: 2}); err != nil {
			t.Fatalf("SaveKnowledgeDocument: %v", err)
		}
		if err := b.SaveKnowledgeDocument(ctx, admin, &interfaces.KnowledgeDocument{ID: otherDoc, OrgID: otherOrg, Filename: "other.md", ChunkerVersion: 1}); err != nil {
			t.Fatalf("SaveKnowledgeDocument(other): %v", err)
		}
		// The vectors point away from any query on purpose: a lexical hit owes nothing to the embedding.
		vec := []float32{0, 1, 0}
		both, underB, legacy, unrelated := docID+"-both", docID+"-b", docID+"-legacy", docID+"-unrelated"
		if err := b.SaveKnowledgeChunks(ctx, admin, []interfaces.KnowledgeChunk{
			{ID: both, DocID: docID, OrgID: orgID, Content: "Never use Run DOS command with admin rights in a production flow.", Embedding: vec, EmbeddingIdentity: "A"},
			{ID: underB, DocID: docID, OrgID: orgID, Content: "Run DOS command needs a timeout.", Embedding: vec, EmbeddingIdentity: "B"},
			{ID: legacy, DocID: docID, OrgID: orgID, Content: "Legacy note: Run DOS command is fine in test flows.", Embedding: vec},
			{ID: unrelated, DocID: docID, OrgID: orgID, Content: "Retry a failed web request three times.", Embedding: vec, EmbeddingIdentity: "A"},
		}); err != nil {
			t.Fatalf("SaveKnowledgeChunks: %v", err)
		}
		if err := b.SaveKnowledgeChunks(ctx, admin, []interfaces.KnowledgeChunk{
			{ID: otherDoc + "-1", DocID: otherDoc, OrgID: otherOrg, Content: "Run DOS command everywhere.", Embedding: vec, EmbeddingIdentity: "A"},
		}); err != nil {
			t.Fatalf("SaveKnowledgeChunks(other): %v", err)
		}

		phrases := []string{"run dos command", "admin rights"}
		hits, err := b.SearchKnowledgeLexical(ctx, orgID, phrases, 10, "A")
		if err != nil || ids(hits) != both+","+legacy {
			t.Errorf("SearchKnowledgeLexical under A = %s, %v; want the chunk matching both phrases, then the legacy one", ids(hits), err)
		}
		if len(hits) > 0 && hits[0].Filename != "lex.md" {
			t.Errorf("a lexical hit names its file %q, want lex.md", hits[0].Filename)
		}
		// The overlap strip reads the chunker version off the hit: a chunk reached only lexically must carry it too.
		if len(hits) > 0 && hits[0].ChunkerVersion != 2 {
			t.Errorf("a lexical hit carries chunker version %d, want its document's 2", hits[0].ChunkerVersion)
		}
		hits, err = b.SearchKnowledgeLexical(ctx, orgID, phrases, 10, "")
		if err != nil || len(hits) != 3 || hits[0].ID != both {
			t.Errorf("SearchKnowledgeLexical without an identity = %s, %v; want the three matching rows of the org, the double match first", ids(hits), err)
		}
		hits, err = b.SearchKnowledgeLexical(ctx, orgID, phrases, 1, "A")
		if err != nil || ids(hits) != both {
			t.Errorf("SearchKnowledgeLexical limited to 1 = %s, %v; want the best", ids(hits), err)
		}
		hits, err = b.SearchKnowledgeLexical(ctx, orgID, []string{"exponential backoff"}, 10, "")
		if err != nil || len(hits) != 0 {
			t.Errorf("SearchKnowledgeLexical of a phrase no chunk holds = %s, %v; want nothing", ids(hits), err)
		}
		hits, err = b.SearchKnowledgeLexical(ctx, orgID, nil, 10, "")
		if err != nil || len(hits) != 0 {
			t.Errorf("SearchKnowledgeLexical without phrases = %s, %v; want nothing", ids(hits), err)
		}
	})

	// The per-phrase cap on candidates is taken among the chunks the caller may see, the best matches first: a crowd
	// of pending chunks, chunks of another identity and older near matches, all stored before it, does not push out
	// the one ready chunk that names the action exactly.
	run("Knowledge_lexical_search_cap_is_not_crowded_out", func(t *testing.T) {
		admin := "contract-kblexc-admin-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: admin, Email: admin + "@example.com", Password: "x", Role: auth.RoleMember}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), admin) })
		orgID := "contract-kblexc-org-" + runID
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: orgID, OwnerID: admin, Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}}); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), orgID) })
		docID := "contract-kblexc-doc-" + runID
		if err := b.SaveKnowledgeDocument(ctx, admin, &interfaces.KnowledgeDocument{ID: docID, OrgID: orgID, Filename: "crowd.md", ChunkerVersion: 1}); err != nil {
			t.Fatalf("SaveKnowledgeDocument: %v", err)
		}
		vec := []float32{0, 1, 0}
		var crowd []interfaces.KnowledgeChunk
		for i := 0; i < 25; i++ {
			crowd = append(crowd,
				interfaces.KnowledgeChunk{ID: fmt.Sprintf("%s-p%02d", docID, i), DocID: docID, OrgID: orgID, Content: "Invoke web service with a timeout.", Pending: true},
				interfaces.KnowledgeChunk{ID: fmt.Sprintf("%s-b%02d", docID, i), DocID: docID, OrgID: orgID, Content: "Invoke web service with a timeout.", Embedding: vec, EmbeddingIdentity: "B"},
				interfaces.KnowledgeChunk{ID: fmt.Sprintf("%s-n%02d", docID, i), DocID: docID, OrgID: orgID, Content: "Invoke web services now and then.", Embedding: vec, EmbeddingIdentity: "A"},
			)
		}
		if err := b.SaveKnowledgeChunks(ctx, admin, crowd); err != nil {
			t.Fatalf("SaveKnowledgeChunks(crowd): %v", err)
		}
		exact := docID + "-z-exact"
		if err := b.SaveKnowledgeChunks(ctx, admin, []interfaces.KnowledgeChunk{{ID: exact, DocID: docID, OrgID: orgID, Content: "Invoke web service needs a retry.", Embedding: vec, EmbeddingIdentity: "A"}}); err != nil {
			t.Fatalf("SaveKnowledgeChunks(exact): %v", err)
		}
		hits, err := b.SearchKnowledgeLexical(ctx, orgID, []string{"invoke web service"}, 1, "A")
		if err != nil || ids(hits) != exact {
			t.Errorf("SearchKnowledgeLexical under a crowd = %s, %v; want the exact ready chunk %s", ids(hits), err, exact)
		}
		hits, err = b.SearchKnowledgeLexical(ctx, orgID, []string{"invoke web service"}, 30, "A")
		for _, h := range hits {
			if strings.Contains(h.ID, "-p") || strings.Contains(h.ID, "-b") {
				t.Errorf("a pending chunk or one of another identity was returned: %s", h.ID)
			}
		}
		if err != nil || len(hits) == 0 || hits[0].ID != exact {
			t.Errorf("SearchKnowledgeLexical(30) = %s, %v; want the exact chunk first", ids(hits), err)
		}
	})

	// Both backends cap a phrase's candidates the same way: its 20 best visible matches by similarity, then by chunk
	// id. A chunk past a phrase's cap scores nothing for that phrase, even if it holds another phrase too.
	run("Knowledge_lexical_search_caps_each_phrase_alike", func(t *testing.T) {
		admin := "contract-kblexk-admin-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: admin, Email: admin + "@example.com", Password: "x", Role: auth.RoleMember}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), admin) })
		orgID := "contract-kblexk-org-" + runID
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: orgID, OwnerID: admin, Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}}); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), orgID) })
		docID := "contract-kblexk-doc-" + runID
		if err := b.SaveKnowledgeDocument(ctx, admin, &interfaces.KnowledgeDocument{ID: docID, OrgID: orgID, Filename: "cap.md", ChunkerVersion: 1}); err != nil {
			t.Fatalf("SaveKnowledgeDocument: %v", err)
		}
		vec := []float32{0, 1, 0}
		var chunks []interfaces.KnowledgeChunk
		for i := 0; i < 21; i++ {
			content := "Invoke web service carefully."
			if i == 20 {
				content = "Invoke web service carefully, with the retry policy."
			}
			chunks = append(chunks, interfaces.KnowledgeChunk{ID: fmt.Sprintf("%s-a%02d", docID, i), DocID: docID, OrgID: orgID, Content: content, Embedding: vec, EmbeddingIdentity: "A"})
		}
		if err := b.SaveKnowledgeChunks(ctx, admin, chunks); err != nil {
			t.Fatalf("SaveKnowledgeChunks: %v", err)
		}
		hits, err := b.SearchKnowledgeLexical(ctx, orgID, []string{"invoke web service", "retry policy"}, 2, "A")
		if err != nil || ids(hits) != docID+"-a00,"+docID+"-a01" {
			t.Errorf("SearchKnowledgeLexical = %s, %v; want a00, a01 (a20 is past the first phrase's cap of 20, so it scores the second phrase alone)", ids(hits), err)
		}
	})
}
