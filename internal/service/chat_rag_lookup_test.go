package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// lookupEmbedder is a knowledge-base embedder for the chat's lookup tests: it names an identity, embeds every text
// as vec (or fails with err, or waits for its context to end when block is set) and records what it embedded.
type lookupEmbedder struct {
	ai.Provider
	identity string
	vec      []float32
	err      error
	block    bool
	mu       sync.Mutex
	inputs   []string
}

func (e *lookupEmbedder) ID() string                { return "lookup-embed" }
func (e *lookupEmbedder) EmbeddingIdentity() string { return e.identity }
func (e *lookupEmbedder) Embed(ctx context.Context, text []string) ([][]float32, error) {
	e.mu.Lock()
	e.inputs = append(e.inputs, text...)
	e.mu.Unlock()
	if e.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = append([]float32(nil), e.vec...)
	}
	return out, nil
}

func (e *lookupEmbedder) embedded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.inputs...)
}

// lookupStore is org-A's knowledge base: one document, one chunk per identity given ("" a legacy row), every
// vector vec.
func lookupStore(vec []float32, identities ...string) *testutil.FakeBackend {
	store := &testutil.FakeBackend{EmbeddingDim: len(vec)}
	store.SeedOrgs("org-A")
	_ = store.SaveKnowledgeDocument(context.Background(), "", &interfaces.KnowledgeDocument{ID: "kb-doc", OrgID: "org-A", Filename: "policy.md"})
	var chunks []interfaces.KnowledgeChunk
	for i, id := range identities {
		chunks = append(chunks, interfaces.KnowledgeChunk{ID: "kb-" + string(rune('a'+i)), DocID: "kb-doc", Content: "Retry policy: three attempts.", Embedding: vec, EmbeddingIdentity: id})
	}
	_ = store.SaveKnowledgeChunks(context.Background(), "", chunks)
	return store
}

// lookupService is a chat service for org-A (user-1 is its member) with knowledge as its knowledge base.
func lookupService(knowledge *rag.KnowledgeService) *ChatService {
	return &ChatService{backend: &memberBackend{}, knowledge: knowledge}
}

func ragOutcomeCount(outcome string) float64 {
	return ragOutcomeCountFrom(ragSourceTurn, outcome)
}

func ragOutcomeCountFrom(source, outcome string) float64 {
	return promtest.ToFloat64(chatRAGLookupsTotal.WithLabelValues(source, outcome))
}

// R-observability-5 and R-knowledge-1/10: every lookup is counted under one outcome that says what happened — the
// membership gate, a question too short to retrieve for, a caller without an embedder or whose embedder cannot see
// the corpus, the deadline, a failure, a miss, a hit — and the outcomes a user can act on carry a knowledge status
// for the turn's notice.
func TestRAGLookup_Outcomes(t *testing.T) {
	orgDoc := &models.FlowDocument{ID: "f1", OrganizationID: "org-A"}
	const question = "what is our retry policy for web requests?"
	noKeys := ai.NewProviderFactory(func(_, _ string) (string, error) { return "", nil }, nil, nil, nil)
	for _, tc := range []struct {
		name, scope, message, outcome, status string
		knowledge                             func() (*rag.KnowledgeService, *lookupEmbedder)
		timeout                               time.Duration
		embeds                                int
		guidelines                            bool
	}{
		{name: "non-member", scope: "user-2", message: question, outcome: "non_member",
			knowledge: func() (*rag.KnowledgeService, *lookupEmbedder) {
				e := &lookupEmbedder{identity: "A", vec: []float32{1}}
				return rag.NewKnowledgeService(lookupStore([]float32{1}, "A"), nil, nil, rag.WithEmbedder(e)), e
			}},
		{name: "trivial", scope: "user-1", message: "ok?", outcome: "trivial",
			knowledge: func() (*rag.KnowledgeService, *lookupEmbedder) {
				e := &lookupEmbedder{identity: "A", vec: []float32{1}}
				return rag.NewKnowledgeService(lookupStore([]float32{1}, "A"), nil, nil, rag.WithEmbedder(e)), e
			}},
		{name: "hit", scope: "user-1", message: question, outcome: "hit", embeds: 1, guidelines: true,
			knowledge: func() (*rag.KnowledgeService, *lookupEmbedder) {
				e := &lookupEmbedder{identity: "A", vec: []float32{1}}
				return rag.NewKnowledgeService(lookupStore([]float32{1}, "A"), nil, nil, rag.WithEmbedder(e)), e
			}},
		{name: "miss", scope: "user-1", message: "how are invoices numbered in the ledger?", outcome: "miss", embeds: 1,
			knowledge: func() (*rag.KnowledgeService, *lookupEmbedder) {
				e := &lookupEmbedder{identity: "A", vec: []float32{0, 1}}
				return rag.NewKnowledgeService(lookupStore([]float32{1, 0}, "A"), nil, nil, rag.WithEmbedder(e)), e
			}},
		{name: "identity mismatch", scope: "user-1", message: question, outcome: "identity_mismatch", status: "identity_mismatch",
			knowledge: func() (*rag.KnowledgeService, *lookupEmbedder) {
				e := &lookupEmbedder{identity: "A", vec: []float32{1}}
				return rag.NewKnowledgeService(lookupStore([]float32{1}, "B", "B"), nil, nil, rag.WithEmbedder(e)), e
			}},
		{name: "no embedder", scope: "user-1", message: question, outcome: "no_embedder", status: "no_embedder",
			knowledge: func() (*rag.KnowledgeService, *lookupEmbedder) {
				return rag.NewKnowledgeService(lookupStore([]float32{1}, "A"), noKeys, nil), &lookupEmbedder{}
			}},
		{name: "deadline", scope: "user-1", message: question, outcome: "deadline", status: "deadline", embeds: 1, timeout: 30 * time.Millisecond,
			knowledge: func() (*rag.KnowledgeService, *lookupEmbedder) {
				e := &lookupEmbedder{identity: "A", block: true}
				return rag.NewKnowledgeService(lookupStore([]float32{1}, "A"), nil, nil, rag.WithEmbedder(e)), e
			}},
		{name: "error", scope: "user-1", message: question, outcome: "error", embeds: 1,
			knowledge: func() (*rag.KnowledgeService, *lookupEmbedder) {
				e := &lookupEmbedder{identity: "A", err: errors.New("provider down")}
				return rag.NewKnowledgeService(lookupStore([]float32{1}, "A"), nil, nil, rag.WithEmbedder(e)), e
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, emb := tc.knowledge()
			svc := lookupService(k)
			ctx := context.Background()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			before := ragOutcomeCount(tc.outcome)
			l := svc.ragGuidelines(ctx, tc.scope, orgDoc, nil, models.ChatRequest{UserMessage: tc.message})
			if d := ragOutcomeCount(tc.outcome) - before; d != 1 {
				t.Errorf("pad_chat_rag_lookups_total{outcome=%q} rose by %v, want 1", tc.outcome, d)
			}
			if l.status != tc.status {
				t.Errorf("knowledge status = %q, want %q", l.status, tc.status)
			}
			if (l.guidelines != "") != tc.guidelines {
				t.Errorf("guidelines = %q, want some: %v", l.guidelines, tc.guidelines)
			}
			if n := len(emb.embedded()); n != tc.embeds {
				t.Errorf("Embed called with %d texts, want %d", n, tc.embeds)
			}
		})
	}
}

// R-knowledge-10: with a block selected, the retrieval query is the question plus a compact description of the
// selection — the action's display name and type, its subflow and its findings' rule ids — so "fix this" retrieves
// what is about that action; without a selection a short question retrieves nothing.
func TestRAGLookup_QueryCarriesTheSelection(t *testing.T) {
	doc := parseCtxBlockDoc(t, ctxBlockSrc)
	doc.OrganizationID = "org-A"
	blockID := blockWithURL(t, doc, "second.example")
	report := &models.AnalysisReport{Findings: []models.Finding{
		{RuleID: "info-rule", Severity: models.SeverityInfo, BlockID: blockID},
		{RuleID: "missing-error-handling", Severity: models.SeverityError, BlockID: blockID},
		{RuleID: "other-block-rule", Severity: models.SeverityError, BlockID: "elsewhere"},
	}}
	emb := &lookupEmbedder{identity: "A", vec: []float32{1}}
	svc := lookupService(rag.NewKnowledgeService(lookupStore([]float32{1}, "A"), nil, nil, rag.WithEmbedder(emb)))

	svc.ragGuidelines(context.Background(), "user-1", doc, report, models.ChatRequest{UserMessage: "fix this", ContextBlockID: blockID})
	got := emb.embedded()
	if len(got) != 1 {
		t.Fatalf("embedded %q, want one query", got)
	}
	q := got[0]
	for _, want := range []string{"fix this", "Invoke Url", "HTTPClient.InvokeUrl", "Main", "missing-error-handling", "info-rule"} {
		if !strings.Contains(q, want) {
			t.Errorf("the retrieval query %q lacks %q", q, want)
		}
	}
	if strings.Contains(q, "other-block-rule") || strings.Contains(q, "second.example") {
		t.Errorf("the retrieval query %q names another block's rule or a property value", q)
	}
	if strings.Index(q, "missing-error-handling") > strings.Index(q, "info-rule") {
		t.Errorf("the retrieval query %q does not name the most severe finding first", q)
	}

	// The same short question without the selection is trivial: nothing is embedded.
	svc.ragGuidelines(context.Background(), "user-1", doc, report, models.ChatRequest{UserMessage: "fix this"})
	if n := len(emb.embedded()); n != 1 {
		t.Errorf("a short question without a selection embedded %d more queries, want 0", n-1)
	}
}

// ragPhaseCount is the number of observations pad_chat_rag_lookup_seconds holds for phase.
func ragPhaseCount(t *testing.T, phase string) uint64 {
	t.Helper()
	var m dto.Metric
	if err := chatRAGLookupSeconds.WithLabelValues(phase).(prometheus.Histogram).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// R-observability-5: a lookup's phases are observed in pad_chat_rag_lookup_seconds{phase}, and every outcome label
// exists before its first lookup, so a dashboard sees a zero rather than no series.
func TestRAGLookup_PhaseHistogramAndPreinitialisedOutcomes(t *testing.T) {
	if n, want := promtest.CollectAndCount(chatRAGLookupsTotal), 3*len(ragLookupOutcomes()); n != want {
		t.Errorf("pad_chat_rag_lookups_total has %d series, want one per source (turn, tool, preview) and outcome (%d)", n, want)
	}
	for _, o := range []string{"non_member", "trivial", "no_embedder", "identity_mismatch", "deadline", "error", "miss", "hit"} {
		if !slices.Contains(ragLookupOutcomes(), o) {
			t.Errorf("outcome %q is missing", o)
		}
	}
	before := map[string]uint64{}
	for _, p := range []string{rag.PhaseResolve, rag.PhaseEmbed, rag.PhaseSearch} {
		before[p] = ragPhaseCount(t, p)
	}
	emb := &lookupEmbedder{identity: "A", vec: []float32{1}}
	svc := lookupService(rag.NewKnowledgeService(lookupStore([]float32{1}, "A"), nil, nil, rag.WithEmbedder(emb)))
	if l := svc.ragGuidelines(context.Background(), "user-1", &models.FlowDocument{ID: "f1", OrganizationID: "org-A"}, nil,
		models.ChatRequest{UserMessage: "what is our retry policy for web requests?"}); l.guidelines == "" {
		t.Fatal("the lookup found nothing")
	}
	for p, n := range before {
		if got := ragPhaseCount(t, p); got != n+1 {
			t.Errorf("phase %s observed %d times, want %d", p, got, n+1)
		}
	}
}

// PAD_RAG_DEADLINE_MS reaches the turn: the lookup a turn starts gives up at the configured deadline, delivers no
// guidelines and the deadline status, however long the knowledge base would take.
func TestRAGLookup_ConfiguredDeadline(t *testing.T) {
	emb := &lookupEmbedder{identity: "A", block: true}
	k := rag.NewKnowledgeService(lookupStore([]float32{1}, "A"), nil, nil, rag.WithEmbedder(emb))
	svc := NewChatService(nil, t.TempDir(), nil, nil, nilSettings{}, nil, &memberBackend{}, "", ChatOptions{Knowledge: k, RAGDeadline: 40 * time.Millisecond})
	if got := svc.ragLookupDeadline(); got != 40*time.Millisecond {
		t.Fatalf("deadline = %v, want the configured 40ms", got)
	}
	if got := (&ChatService{}).ragLookupDeadline(); got != ragGuidelinesDeadline {
		t.Errorf("an unconfigured service's deadline = %v, want the default %v", got, ragGuidelinesDeadline)
	}

	before := ragOutcomeCount("deadline")
	start := time.Now()
	h := svc.startRAGLookup(context.Background(), "user-1", &models.FlowDocument{ID: "f1", OrganizationID: "org-A"}, nil,
		models.ChatRequest{UserMessage: "what is our retry policy for web requests?"})
	l, ok := h.wait(context.Background())
	if !ok || l.guidelines != "" || l.status != "deadline" {
		t.Errorf("lookup = %+v, %v; want no guidelines and the deadline status", l, ok)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the lookup took %v under a 40ms deadline", took)
	}
	if h.status() != "deadline" {
		t.Errorf("status after delivery = %q, want deadline", h.status())
	}
	if d := ragOutcomeCount("deadline") - before; d != 1 {
		t.Errorf("deadline outcome rose by %v, want 1", d)
	}
}

// A search failure is a WARN, at most one per org a minute: a member without an embedder fails every turn.
func TestRAGLookup_WarnRateLimitedPerOrg(t *testing.T) {
	svc := &ChatService{}
	if !svc.ragWarnAllowed("org-1") {
		t.Fatal("the first warning of an org was refused")
	}
	if svc.ragWarnAllowed("org-1") {
		t.Error("a second warning of the same org within the interval was allowed")
	}
	if !svc.ragWarnAllowed("org-2") {
		t.Error("another org's first warning was refused")
	}
}

// R-knowledge-1: a turn whose knowledge lookup the user can act on says so once, before the first chunk, in a
// journaled notice {kind: knowledge, status}; a turn that found guidelines, or missed, says nothing.
func TestStreamTurn_KnowledgeNotice(t *testing.T) {
	registerFakeProvider()
	run := func(t *testing.T, store *testutil.FakeBackend, emb *lookupEmbedder) []map[string]interface{} {
		t.Helper()
		backend := &turnReadsBackend{FakeBackend: testutil.NewFakeBackend()}
		svc := &ChatService{
			notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
			factory:  ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil),
			settings: nilSettings{}, backend: backend, chatCtxCache: newChatContextCache(),
			knowledge: rag.NewKnowledgeService(store, nil, nil, rag.WithEmbedder(emb)),
		}
		doc := &models.FlowDocument{ID: "f1", Name: "f1", OrganizationID: "org-A"}
		id, err := svc.StreamChatMessage(asUser("u1"), "u1", doc, nil, models.ChatRequest{
			Provider: "fake", Model: "fake-model", UserMessage: "what is our retry policy for web requests?", ClientStreamID: uuid.NewString(),
		})
		if err != nil {
			t.Fatalf("StreamChatMessage: %v", err)
		}
		waitStreamFinished(t, svc, id)
		var out []map[string]interface{}
		for _, ev := range svc.notifier.(*testutil.CountingNotifier).Events() {
			if env, ok := ev.Data.(map[string]interface{}); ok && ev.Name == "chat:event" {
				out = append(out, env)
			}
		}
		return out
	}
	notices := func(envs []map[string]interface{}) (knowledge []map[string]interface{}, beforeChunk bool) {
		beforeChunk = true
		sawChunk := false
		for _, env := range envs {
			data, _ := env["data"].(map[string]interface{})
			switch env["type"] {
			case "chunk":
				sawChunk = true
			case "notice":
				if data["kind"] == "knowledge" {
					knowledge = append(knowledge, data)
					beforeChunk = beforeChunk && !sawChunk
				}
			}
		}
		return knowledge, beforeChunk
	}

	t.Run("identity mismatch", func(t *testing.T) {
		emb := &lookupEmbedder{identity: "A", vec: []float32{1}}
		got, before := notices(run(t, lookupStore([]float32{1}, "B"), emb))
		if len(got) != 1 || got[0]["status"] != "identity_mismatch" {
			t.Fatalf("knowledge notices = %v, want one with status identity_mismatch", got)
		}
		if !before {
			t.Error("the knowledge notice came after the first chunk")
		}
		if n := len(emb.embedded()); n != 0 {
			t.Errorf("the turn embedded %d queries for a corpus its member cannot see, want 0", n)
		}
	})
	t.Run("hit", func(t *testing.T) {
		got, _ := notices(run(t, lookupStore([]float32{1}, "A"), &lookupEmbedder{identity: "A", vec: []float32{1}}))
		if len(got) != 0 {
			t.Errorf("a turn with guidelines emitted knowledge notices %v", got)
		}
	})
}
