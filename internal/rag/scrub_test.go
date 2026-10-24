package rag

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/storage/interfaces"
	tu "pad-analyzer/internal/testutil"
	"pad-core/ai/scrubber"
)

// A connection-string password, assembled so no credential scanner matches the test source.
func testSecret() string { return "hunter2x" + "Q9longvalue" }

func scrubbedChunks(source string) float64 {
	return testutil.ToFloat64(ragScrubbedChunksTotal.WithLabelValues(source))
}

// TestAddDocument_MasksSecretsBeforeEmbeddingAndStorage: a runbook holding a connection-string password reaches
// neither the embedding provider nor the store with the value — the embed request body and the stored chunk (and
// the stored filename) carry the mask — and the masked chunk is counted.
func TestAddDocument_MasksSecretsBeforeEmbeddingAndStorage(t *testing.T) {
	secret := testSecret()
	content := "Connect to the warehouse with Server=db;User=svc;Password=" + secret + ";Encrypt=true and retry three times.\n\nEscalate after the third failure."
	emb := &recordingEmbedder{}
	store := &tu.FakeBackend{}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))
	before := scrubbedChunks("upload")

	if err := svc.AddDocument(context.Background(), "scope", "org-1", "Password="+secret+".md", content); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	if len(emb.inputs) != 1 {
		t.Fatalf("embedded %d inputs, want 1", len(emb.inputs))
	}
	if got := emb.inputs[0]; strings.Contains(got, secret) || !strings.Contains(got, "Password=[REDACTED];Encrypt=true") || !strings.Contains(got, "retry three times") {
		t.Errorf("embed request body = %q, want the password masked and the rest of the runbook kept", got)
	}
	if n := len(store.KnowledgeChunks); n != 1 {
		t.Fatalf("stored %d chunks, want 1", n)
	}
	for _, c := range store.KnowledgeChunks {
		if strings.Contains(c.Content, secret) || !strings.Contains(c.Content, "Password=[REDACTED]") {
			t.Errorf("stored chunk = %q, want the password masked", c.Content)
		}
	}
	for _, d := range store.KnowledgeDocs {
		if strings.Contains(d.Filename, secret) {
			t.Errorf("stored filename carries the secret: %q", d.Filename)
		}
	}
	if d := scrubbedChunks("upload") - before; d != 1 {
		t.Errorf(`pad_rag_scrubbed_chunks_total{source="upload"} rose by %v, want 1`, d)
	}
}

// TestAddDocument_CleanDocumentIsNotCounted: a document without a secret pattern is stored as sent and counts nothing.
func TestAddDocument_CleanDocumentIsNotCounted(t *testing.T) {
	emb := &recordingEmbedder{}
	store := &tu.FakeBackend{}
	store.SeedOrgs("org-1")
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))
	before := scrubbedChunks("upload")
	const content = "Retry three times, then escalate to the on-call engineer."
	if err := svc.AddDocument(context.Background(), "scope", "org-1", "retry-policy.md", content); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	for _, c := range store.KnowledgeChunks {
		if c.Content != content {
			t.Errorf("stored chunk = %q, want the text unchanged", c.Content)
		}
	}
	if d := scrubbedChunks("upload") - before; d != 0 {
		t.Errorf(`pad_rag_scrubbed_chunks_total{source="upload"} rose by %v, want 0`, d)
	}
}

// carriedHead is the part of a chunk that can be the overlap carried from its predecessor: its first chunkOverlapRunes.
func carriedHead(chunk string) string {
	r := []rune(chunk)
	return string(r[:min(chunkOverlapRunes, len(r))])
}

// secretStraddlingOverlap builds a one-paragraph document in which a connection-string password lies in the tail
// chunkText carries from one chunk into the next, cut so that the carried copy begins INSIDE the secret: scrubbed
// chunk by chunk, the first chunk masks it and the second keeps a key-less fragment of the value, and the two
// chunks no longer share an identical overlap. The placement also keeps the mask inside the carried overlap once
// the document is scrubbed whole (the mask is shorter than the value, so the split moves). Fails the test when no
// placement produces both (fixture rot).
func secretStraddlingOverlap(t *testing.T) (text string, raw []string, at int) {
	t.Helper()
	secret := testSecret()
	line := "Then connect with Server=db;User=svc;Password=" + secret + ";Encrypt=true."
	filler := func(i int) string { return fmt.Sprintf("Guideline number %03d applies to every flow in the team.", i) }
	for m := 12; m < 24; m++ {
		for pad := 0; pad < 60; pad++ {
			var sb strings.Builder
			sb.WriteString("Preface " + strings.Repeat("x", pad) + ".")
			for i := range 60 {
				sb.WriteByte(' ')
				if i == m {
					sb.WriteString(line)
				}
				sb.WriteString(filler(i))
			}
			text := sb.String()
			raw := chunkText(text, knowledgeChunkRunes)
			cut := -1
			for i := 0; i+1 < len(raw); i++ {
				head := carriedHead(raw[i+1])
				if strings.Contains(raw[i], "Password="+secret) && strings.Contains(head, secret[len(secret)/2:]) && !strings.Contains(head, "Password="+secret) {
					cut = i
				}
			}
			if cut < 0 {
				continue
			}
			masked := chunkText(scrubber.ScrubText(text), knowledgeChunkRunes)
			for j := 0; j+1 < len(masked); j++ {
				if strings.Contains(masked[j], "Password=[REDACTED]") && strings.Contains(carriedHead(masked[j+1]), "Password=[REDACTED]") {
					return text, raw, cut
				}
			}
		}
	}
	t.Fatal("fixture rot: no placement cut the carried overlap inside the secret and kept the mask in it once scrubbed")
	return "", nil, 0
}

// TestAddDocument_ScrubBeforeChunkingKeepsOverlapIdentical pins WHY the document is scrubbed before it is chunked
// rather than chunk by chunk: a secret straddling the carried overlap is then masked once, in the text every chunk
// is cut from, so the neighbours share an identical overlap and stripOverlapPrefix still de-duplicates it, and no
// chunk keeps a fragment of the value.
func TestAddDocument_ScrubBeforeChunkingKeepsOverlapIdentical(t *testing.T) {
	secret := testSecret()
	text, raw, at := secretStraddlingOverlap(t)

	// The premise: scrubbed chunk by chunk, the second chunk keeps a fragment of the value and the pair no longer
	// de-duplicates.
	a, b := scrubber.ScrubText(raw[at]), scrubber.ScrubText(raw[at+1])
	perChunk := []interfaces.KnowledgeChunk{{DocID: "d", Content: a}, {DocID: "d", Content: b}}
	if !strings.Contains(b, secret[len(secret)/2:]) {
		t.Fatalf("fixture rot: per-chunk scrub left no fragment in %q", b)
	}
	if stripOverlapPrefix(b, "d", perChunk) != b {
		t.Fatal("fixture rot: per-chunk scrubbing still de-duplicated the overlap")
	}

	emb := &recordingEmbedder{}
	store := &tu.FakeBackend{}
	store.SeedOrgs("org-1")
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))
	if err := svc.AddDocument(context.Background(), "scope", "org-1", "runbook.md", text); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	for _, in := range emb.inputs {
		if strings.Contains(in, secret[len(secret)/2:]) {
			t.Errorf("embed request body carries a fragment of the secret: %q", in)
		}
	}
	var stored []interfaces.KnowledgeChunk
	for _, c := range store.KnowledgeChunks {
		if strings.Contains(c.Content, secret[len(secret)/2:]) {
			t.Errorf("stored chunk carries a fragment of the secret: %q", c.Content)
		}
		stored = append(stored, c)
	}
	// Rendered together, the masked line appears once: the carried copy is stripped as an exact overlap.
	withMask, afterStrip := 0, 0
	for _, c := range stored {
		if strings.Contains(c.Content, "Password=[REDACTED]") {
			withMask++
			if strings.Contains(stripOverlapPrefix(c.Content, c.DocID, stored), "Password=[REDACTED]") {
				afterStrip++
			}
		}
	}
	if withMask != 2 || afterStrip != 1 {
		t.Errorf("chunks with the mask: %d (want 2, the line and its carried copy); after overlap stripping: %d (want 1)", withMask, afterStrip)
	}
}

// TestReindexOrg_ReembedsLegacyChunkFromScrubbedText: a chunk stored raw before uploads were masked is re-embedded
// from its scrubbed text — the value never reaches the provider again — and counted under the reindex source; a
// clean chunk goes as stored.
func TestReindexOrg_ReembedsLegacyChunkFromScrubbedText(t *testing.T) {
	secret := testSecret()
	store := &tu.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1")
	seedCorpus(store, []float32{1}, "Connect with Password="+secret+"; then retry.", "plain guideline") // legacy: no identity
	emb := &recordingEmbedder{}
	svc := guardService(store, emb)
	before := scrubbedChunks("reindex")

	res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
	if err != nil {
		t.Fatalf("ReindexOrg: %v", err)
	}
	if res.Chunks != 2 {
		t.Fatalf("re-indexed %d chunks, want 2", res.Chunks)
	}
	masked, plain := false, false
	for _, in := range emb.inputs {
		if strings.Contains(in, secret) {
			t.Errorf("re-index embed request carried the secret: %q", in)
		}
		masked = masked || in == "Connect with Password=[REDACTED]; then retry."
		plain = plain || in == "plain guideline"
	}
	if !masked || !plain {
		t.Errorf("embed inputs = %q, want the legacy chunk masked and the clean one as stored", emb.inputs)
	}
	if d := scrubbedChunks("reindex") - before; d != 1 {
		t.Errorf(`pad_rag_scrubbed_chunks_total{source="reindex"} rose by %v, want 1`, d)
	}
}

// TestCorpusModelGuard_EmbedsScrubbedSamples: the mismatch guard re-embeds sampled stored text; a legacy sample
// holding a secret goes to the provider masked.
func TestCorpusModelGuard_EmbedsScrubbedSamples(t *testing.T) {
	secret := testSecret()
	store := &tu.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1")
	seedCorpus(store, []float32{1}, "Connect with Password="+secret+"; then retry.")
	emb := &recordingEmbedder{}
	svc := guardService(store, emb)

	// A second document joins the corpus: the guard samples the first.
	if err := svc.AddDocument(context.Background(), "scope", "org-1", "other.md", "Escalate after the third failure."); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	sampled := false
	for _, in := range emb.inputs {
		if strings.Contains(in, secret) {
			t.Errorf("the guard's sample embed carried the secret: %q", in)
		}
		sampled = sampled || strings.Contains(in, "Password=[REDACTED]")
	}
	if !sampled {
		t.Errorf("embed inputs = %q, want the legacy sample among them, masked", emb.inputs)
	}
}

// A credential literal in pasted Power Automate Desktop code, assembled so no credential scanner matches the test
// source: the regex pass alone leaves a SET of a credential variable intact; scrubber.ScrubFreeText masks it.
func testFlowSecret() string { return "Winter#2024-" + "u7Qx" }

func flowRunbook() string {
	return "Set the connection up the way the login flow does:\n\nSET Password TO $'''" + testFlowSecret() + "'''\n\nThen retry three times."
}

// TestAddDocument_MasksPastedFlowCodeLiterals: a runbook that pastes a PAD action with a credential literal reaches
// neither the embedding provider nor the store with the value — PAD runbooks are the product's domain, so the
// knowledge base scrubs with ScrubFreeText, which knows PAD's literal syntax, not the regex pass alone.
func TestAddDocument_MasksPastedFlowCodeLiterals(t *testing.T) {
	emb := &recordingEmbedder{}
	store := &tu.FakeBackend{}
	store.SeedOrgs("org-1")
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))
	before := scrubbedChunks("upload")

	if err := svc.AddDocument(context.Background(), "scope", "org-1", "login-flow.md", flowRunbook()); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	if len(emb.inputs) != 1 {
		t.Fatalf("embedded %d inputs, want 1", len(emb.inputs))
	}
	if got := emb.inputs[0]; strings.Contains(got, testFlowSecret()) || !strings.Contains(got, "SET Password TO $'''[REDACTED]'''") || !strings.Contains(got, "retry three times") {
		t.Errorf("embed request body = %q, want the literal masked and the rest of the runbook kept", got)
	}
	for _, c := range store.KnowledgeChunks {
		if strings.Contains(c.Content, testFlowSecret()) || !strings.Contains(c.Content, "SET Password TO $'''[REDACTED]'''") {
			t.Errorf("stored chunk = %q, want the literal masked", c.Content)
		}
	}
	if d := scrubbedChunks("upload") - before; d != 1 {
		t.Errorf(`pad_rag_scrubbed_chunks_total{source="upload"} rose by %v, want 1`, d)
	}
}

// TestReindexOrg_ReembedsLegacyFlowCodeFromScrubbedText: a legacy chunk holding a pasted PAD credential literal is
// re-embedded with the literal masked.
func TestReindexOrg_ReembedsLegacyFlowCodeFromScrubbedText(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1")
	seedCorpus(store, []float32{1}, flowRunbook()) // legacy: no identity
	emb := &recordingEmbedder{}
	svc := guardService(store, emb)

	res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
	if err != nil || res.Chunks != 1 {
		t.Fatalf("ReindexOrg = %+v, %v; want 1 chunk", res, err)
	}
	for _, in := range emb.inputs {
		if strings.Contains(in, testFlowSecret()) || !strings.Contains(in, "SET Password TO $'''[REDACTED]'''") {
			t.Errorf("re-index embed request = %q, want the literal masked", in)
		}
	}
}

// TestSearch_ScrubsPastedFlowCodeInQuery: the query sent to the embedding provider is scrubbed like the documents —
// a member who pastes the failing action into the question does not hand its literal to the vendor.
func TestSearch_ScrubsPastedFlowCodeInQuery(t *testing.T) {
	emb := &recordingEmbedder{}
	store := &tu.FakeBackend{}
	store.SeedOrgs("org-1")
	seedCorpus(store, []float32{1}, "retry policy: three attempts")
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))

	if _, err := svc.Search(context.Background(), "scope", "org-1", "why does this fail?\nSET Password TO $'''"+testFlowSecret()+"'''"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(emb.inputs) != 1 {
		t.Fatalf("embedded %d inputs, want 1", len(emb.inputs))
	}
	if got := emb.inputs[0]; strings.Contains(got, testFlowSecret()) || !strings.Contains(got, "SET Password TO $'''[REDACTED]'''") {
		t.Errorf("query embed request = %q, want the literal masked", got)
	}
}
