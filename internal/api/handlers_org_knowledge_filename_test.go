package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// countingEmbedder is a directionEmbedder that counts its Embed calls: a refused upload must not reach the
// (paid) embeddings API.
type countingEmbedder struct {
	directionEmbedder
	calls *atomic.Int32
}

func (c countingEmbedder) Embed(ctx context.Context, text []string) ([][]float32, error) {
	c.calls.Add(1)
	return c.directionEmbedder.Embed(ctx, text)
}

// newKnowledgeFilenameRouter serves the knowledge routes for an org (admin = the local-mode caller) with an empty
// knowledge base, embedding through an embedder that counts its calls.
func newKnowledgeFilenameRouter(t *testing.T) (http.Handler, *testutil.FakeBackend, string, *atomic.Int32) {
	t.Helper()
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &testutil.FakeBackend{}
	store.SeedOrgs(org.ID) // the knowledge base needs its organisation
	calls := &atomic.Int32{}
	knowledge := rag.NewKnowledgeService(store, nil, nil, rag.WithEmbedder(countingEmbedder{directionEmbedder: directionEmbedder{vec: []float32{1, 0}}, calls: calls}))
	t.Cleanup(knowledge.StopJobs)
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil)
	return mountedAs(h, "u-admin"), store, org.ID, calls
}

// uploadBody is the JSON upload body for filename with a fixed content.
func uploadBody(t *testing.T, filename string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"filename": filename, "content": "retry every call three times"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestKnowledgeUpload_InvalidFilenameRefused (R-security-5): the filename is the replace key, is stored unbounded and
// is rendered into the prompt, so the handler refuses an empty, oversized, non-UTF-8, control-bearing or path-bearing
// name with 400 KNOWLEDGE_FILENAME_INVALID before any embedding call, and stores nothing.
func TestKnowledgeUpload_InvalidFilenameRefused(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty", uploadBody(t, "")},
		{"blank", uploadBody(t, "  ")},
		{"newline", uploadBody(t, "a\nb")},
		{"tab", uploadBody(t, "a\tb.md")},
		{"DEL", uploadBody(t, "a\x7fb.md")},
		{"C1 control", uploadBody(t, "a\u0085b.md")},
		{"parent path", uploadBody(t, "../x.md")},
		{"slash", uploadBody(t, "dir/x.md")},
		{"backslash", uploadBody(t, `a\b.md`)},
		// encoding/json decodes invalid UTF-8 (raw bytes or a lone surrogate escape) to U+FFFD, so the
		// handler sees the replacement character, never the invalid bytes.
		{"invalid UTF-8 bytes", `{"filename":"a` + "\xff\xfe" + `.md","content":"retry every call three times"}`},
		{"lone surrogate", `{"filename":"a\ud800.md","content":"retry every call three times"}`},
		{"300 bytes", uploadBody(t, strings.Repeat("a", 297)+".md")},
		{"256 bytes", uploadBody(t, strings.Repeat("a", 253)+".md")},
		{"256 bytes multibyte", uploadBody(t, strings.Repeat("é", 126)+".md.")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store, orgID, calls := newKnowledgeFilenameRouter(t)
			rr := postJSON(t, h, "/api/orgs/"+orgID+"/knowledge/upload", tc.body)
			if rr.Code != http.StatusBadRequest || errorCode(t, rr) != "KNOWLEDGE_FILENAME_INVALID" {
				t.Fatalf("status = %d, body = %s; want 400 KNOWLEDGE_FILENAME_INVALID", rr.Code, rr.Body.String())
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("refused upload made %d embed calls, want 0", n)
			}
			if len(store.KnowledgeDocs) != 0 {
				t.Errorf("refused upload stored %d documents", len(store.KnowledgeDocs))
			}
		})
	}
}

// TestKnowledgeUpload_ValidFilenameUploads: names at the edges of the rule still upload — 255 bytes exactly,
// non-ASCII letters, inner spaces and dots.
func TestKnowledgeUpload_ValidFilenameUploads(t *testing.T) {
	for _, name := range []string{
		"policy.md",
		"Naming rules v2.final.md",
		"Richtlinien für Flüsse.md",
		strings.Repeat("a", 252) + ".md", // 255 bytes
	} {
		h, store, orgID, calls := newKnowledgeFilenameRouter(t)
		rr := postJSON(t, h, "/api/orgs/"+orgID+"/knowledge/upload", uploadBody(t, name))
		if rr.Code != http.StatusCreated {
			t.Fatalf("upload %q: status = %d, body = %s", name, rr.Code, rr.Body.String())
		}
		if calls.Load() == 0 {
			t.Errorf("upload %q made no embed call", name)
		}
		if len(store.KnowledgeDocs) != 1 {
			t.Errorf("upload %q stored %d documents, want 1", name, len(store.KnowledgeDocs))
		}
	}
}

// TestValidKnowledgeFilename pins the rule on strings JSON decoding never produces (invalid UTF-8 survives only
// outside encoding/json).
func TestValidKnowledgeFilename(t *testing.T) {
	// Format and invisible characters (bidi overrides, zero-width marks, the BOM) and the Unicode line separators
	// would reach the prompt verbatim inside the "[name]" attribution of every retrieved chunk.
	for _, name := range []string{"a\xffb.md", "\xc3", "a\x00b.md",
		"report\u202Edm.txt", "po\u200Blicy.md", "\uFEFFpolicy.md", "a\u2028b.md", "a\u2029b.md", "\u2066policy.md", "policy\u200D.md"} {
		if validKnowledgeFilename(name) == nil {
			t.Errorf("validKnowledgeFilename(%q) = nil, want an error", name)
		}
	}
	for _, name := range []string{"policy.md", "Policy [v2].md", "Richtlinien – Überblick.md", "手順書.md"} {
		if err := validKnowledgeFilename(name); err != nil {
			t.Errorf("validKnowledgeFilename(%q) = %v, want nil", name, err)
		}
	}
}
