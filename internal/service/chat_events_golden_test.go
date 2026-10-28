package service

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// The chat event contract (R-testing-3): the backend is the source of truth for the SSE envelopes the client parses
// ({streamId, type, data}) and for the /api/chat/resume body. This test drives real streams and the real applier,
// and writes one envelope per event variant to testdata/chat_events/; frontend/src/lib/chatEvent.contract.test.ts
// parses every file and fails when an event parses to null or drops a field the backend sends. Refresh with
// go test ./internal/service/ -run TestChatEvents_Golden -update-chat-events, and review the diff.
var updateChatEvents = flag.Bool("update-chat-events", false, "rewrite internal/service/testdata/chat_events/")

const chatEventsDir = "testdata/chat_events"

// envelopeRecorder keeps the first envelope of each variant name.
type envelopeRecorder struct {
	mu   sync.Mutex
	seen map[string]map[string]interface{}
}

func (r *envelopeRecorder) keep(name string, env map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[string]map[string]interface{}{}
	}
	if _, ok := r.seen[name]; !ok {
		r.seen[name] = env
	}
}

// variant names an envelope: its type, and for a decision its status (batch decisions apart).
func variant(typ string, data map[string]interface{}) string {
	switch typ {
	case "fix_proposal":
		if data["batch"] == true {
			return "fix_proposal_batch"
		}
		return "fix_proposal_single"
	case "fix_decision":
		st, _ := data["status"].(string)
		if _, ok := data["items"]; ok {
			return "fix_decision_batch_" + st
		}
		return "fix_decision_" + st
	case "notice":
		k, _ := data["kind"].(string)
		return "notice_" + k
	case "tool_result":
		if data["name"] == journalTruncatedName {
			return "tool_result_journal_truncated"
		}
	}
	return typ
}

// streamEnvelopes keeps every chat:event the notifier carried.
func streamEnvelopes(rec *envelopeRecorder, n *testutil.CountingNotifier) {
	for _, ev := range n.Events() {
		if ev.Name != "chat:event" {
			continue
		}
		env, ok := ev.Data.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := env["type"].(string)
		data, _ := env["data"].(map[string]interface{})
		rec.keep(variant(typ, data), env)
	}
}

// applierEnvelopes is an applier emit that wraps each event in the stream's envelope, as streamEmitter does.
func applierEnvelopes(rec *envelopeRecorder) func(string, map[string]interface{}) {
	return func(typ string, data map[string]interface{}) {
		rec.keep(variant(typ, data), map[string]interface{}{"streamId": "stream-1", "type": typ, "data": data})
	}
}

func TestChatEvents_Golden(t *testing.T) {
	rec := &envelopeRecorder{}
	ctx := context.Background()

	// A tool turn: a read tool, an approved fix, the answer.
	doc := fixTestDoc()
	p := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "list_findings", Input: []byte(`{}`)}}, TokensIn: 10, TokensOut: 5},
		{ToolCalls: []ai.ToolCall{{ID: "t2", Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}}, TokensIn: 12, TokensOut: 5},
		{Text: "Fixed it.", TokensIn: 8, TokensOut: 3, ServedModel: "fake-model-2026-10"},
	}}
	svc := failedTurnService(t, "chat-events-golden", p, doc)
	_, ctl := streamTurn(t, svc, doc, "chat-events-golden", true)
	if !ctl.done {
		t.Fatalf("the tool turn failed: %s", ctl.errMsg)
	}
	streamEnvelopes(rec, svc.notifier.(*testutil.CountingNotifier))
	res, err := svc.ResumeStream(ctx, ctl.streamID, 0)
	if err != nil {
		t.Fatal(err)
	}

	// A failure the provider billed (the output cap hit before any text): the error carries its usage.
	p2 := &testutil.FakeProvider{Turns: []testutil.FakeTurn{{FinishReason: "length", TokensIn: 30, TokensOut: 4096}}}
	svc2 := failedTurnService(t, "chat-events-golden-error", p2, doc)
	if _, ctl2 := streamTurn(t, svc2, doc, "chat-events-golden-error", false); ctl2.errMsg == "" {
		t.Fatal("the failing turn did not fail")
	}
	streamEnvelopes(rec, svc2.notifier.(*testutil.CountingNotifier))

	// The history notice: a long stored conversation on a small window.
	p3 := &testutil.FakeProvider{Ctx: 2000, Turns: []testutil.FakeTurn{{Text: "answer", TokensIn: 3, TokensOut: 2}}}
	svc3 := newCtxBlockChatService(t, "chat-events-golden-notice", p3)
	ndoc := parseCtxBlockDoc(t, ctxBlockSrc)
	var history []models.ChatMessage
	for i := 0; i < 12; i++ {
		history = append(history,
			models.ChatMessage{ID: uuid.NewString(), Role: "user", Content: fmt.Sprintf("question %d: %s", i, strings.Repeat("why does the flow fail here ", 16))},
			models.ChatMessage{ID: uuid.NewString(), Role: "assistant", Content: fmt.Sprintf("answer %d: %s", i, strings.Repeat("because the loop never exits ", 16))})
	}
	nreq := models.ChatRequest{Provider: "chat-events-golden-notice", Model: "fake-model", UserMessage: "and now?", ConversationID: uuid.NewString(), ClientStreamID: uuid.NewString()}
	if _, err := svc3.saveConversation(ctx, ndoc.ID, nreq.ConversationKey(), history); err != nil {
		t.Fatal(err)
	}
	nid, err := svc3.StreamChatMessage(ctx, "u1", ndoc, nil, nreq)
	if err != nil {
		t.Fatal(err)
	}
	waitStreamFinished(t, svc3, nid)
	streamEnvelopes(rec, svc3.notifier.(*testutil.CountingNotifier))

	// The knowledge notice: an org flow whose knowledge base the member's embedding model cannot see.
	registerFakeProvider()
	svc4 := &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory:  ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil),
		settings: nilSettings{}, backend: &turnReadsBackend{FakeBackend: testutil.NewFakeBackend()}, chatCtxCache: newChatContextCache(),
		knowledge: rag.NewKnowledgeService(lookupStore([]float32{1}, "B"), nil, nil, rag.WithEmbedder(&lookupEmbedder{identity: "A", vec: []float32{1}})),
	}
	kid, err := svc4.StreamChatMessage(asUser("u1"), "u1", &models.FlowDocument{ID: "f-kb", Name: "f-kb", OrganizationID: "org-A"}, nil, models.ChatRequest{
		Provider: "fake", Model: "fake-model", UserMessage: "what is our retry policy for web requests?", ClientStreamID: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitStreamFinished(t, svc4, kid)
	streamEnvelopes(rec, svc4.notifier.(*testutil.CountingNotifier))

	// A heartbeat: the keepalive of a stream whose emits paused.
	hbCtl := &streamCtl{started: make(chan struct{})}
	close(hbCtl.started)
	hbStop := make(chan struct{})
	beat := make(chan struct{})
	var once sync.Once
	go heartbeatStream(ctx, hbCtl, func(typ string, data map[string]interface{}) {
		rec.keep(variant(typ, data), map[string]interface{}{"streamId": "stream-1", "type": typ, "data": data})
		once.Do(func() { close(beat) })
	}, hbStop, time.Millisecond, time.Millisecond)
	select {
	case <-beat:
	case <-time.After(3 * time.Second):
		t.Error("no heartbeat")
	}
	close(hbStop)

	// Every other card outcome, through the applier.
	decisionEnvelopes(t, rec)

	// The journal's truncation marker (R-streaming-2): replayed to a reconnecting client, never emitted live.
	trimmed := &streamCtl{}
	trimmed.mu.Lock()
	for i := 0; i <= maxJournalEvents; i++ {
		trimmed.recordJournal("tool_result", map[string]interface{}{"name": "list_findings"})
	}
	for _, ev := range trimmed.snapshotEvents() {
		rec.keep(variant(ev.Type, ev.Data), map[string]interface{}{"streamId": "stream-1", "type": ev.Type, "data": ev.Data})
	}
	trimmed.mu.Unlock()

	want := []string{
		"chunk", "done", "error", "heartbeat", "tool", "tool_result", "notice_history_truncated", "notice_knowledge",
		"fix_proposal_single", "fix_proposal_batch",
		"fix_decision_applying", "fix_decision_applied", "fix_decision_already-resolved", "fix_decision_declined",
		"fix_decision_timeout", "fix_decision_cancelled", "fix_decision_error", "fix_decision_batch_applied",
		"tool_result_journal_truncated",
	}
	files := map[string][]byte{}
	for _, name := range want {
		env, ok := rec.seen[name]
		if !ok {
			t.Errorf("no %s envelope was emitted", name)
			continue
		}
		files[name+".json"] = goldenJSON(t, env)
	}
	files["resume.json"] = goldenJSON(t, res)

	if *updateChatEvents {
		if err := os.RemoveAll(chatEventsDir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(chatEventsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(chatEventsDir, name), data, 0o644); err != nil { // #nosec G306 -- a checked-in fixture
				t.Fatal(err)
			}
		}
		return
	}
	entries, _ := os.ReadDir(chatEventsDir)
	var onDisk []string
	for _, e := range entries {
		onDisk = append(onDisk, e.Name())
	}
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	if strings.Join(onDisk, ",") != strings.Join(names, ",") {
		t.Errorf("chat event goldens on disk %v, want %v (-update-chat-events)", onDisk, names)
	}
	for _, name := range names {
		got, err := os.ReadFile(filepath.Join(chatEventsDir, name))
		if err != nil {
			continue
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r"), nil), files[name]) {
			t.Errorf("%s differs from what the backend emits now (-update-chat-events):\n%s", name, files[name])
		}
	}
}

// decisionEnvelopes runs the applier through the outcomes a tool turn does not reach.
func decisionEnvelopes(t *testing.T, rec *envelopeRecorder) {
	t.Helper()
	ctx := context.Background()
	doc := fixTestDoc()
	decide := func(svc *ChatService, approved bool, excluded []int) {
		go func() {
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				done := false
				svc.pendingFixDecisions.Range(func(k, _ any) bool {
					_ = svc.ResolveFixDecision("stream-1", k.(string), approved, excluded)
					done = true
					return false
				})
				if done {
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}
	fixed := func(_ context.Context, d *models.FlowDocument, _, _, _, _, _ string) (*models.FlowDocument, error) {
		return commentDoc(d.ID), nil
	}

	// declined
	svc, _ := newFixTestService(doc, fixed)
	decide(svc, false, nil)
	_, _ = svc.newFixApplier(doc, "stream-1", applierEnvelopes(rec), nil, nil).ApplyFixWithApproval(ctx, fixTestProposal())

	// timeout
	svc, _ = newFixTestService(doc, fixed)
	svc.fixDecisionWait = 10 * time.Millisecond
	_, _ = svc.newFixApplier(doc, "stream-1", applierEnvelopes(rec), nil, nil).ApplyFixWithApproval(ctx, fixTestProposal())

	// cancelled
	svc, _ = newFixTestService(doc, fixed)
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		waitPending(t, svc, fixTestProposal().ProposalID)
		cancel()
	}()
	_, _ = svc.newFixApplier(doc, "stream-1", applierEnvelopes(rec), nil, nil).ApplyFixWithApproval(cctx, fixTestProposal())

	// already-resolved: the finding is gone by the approval
	svc, ldp := newReassocService(doc, fixed)
	go func() {
		waitPending(t, svc, fixTestProposal().ProposalID)
		ldp.SetCurrentDoc(commentDoc(doc.ID))
		_ = svc.ResolveFixDecision("stream-1", fixTestProposal().ProposalID, true, nil)
	}()
	_, _ = svc.newFixApplier(doc, "stream-1", applierEnvelopes(rec), nil, nil).ApplyFixWithApproval(ctx, fixTestProposal())

	// error: the write fails
	svc, _ = newFixTestService(doc, func(context.Context, *models.FlowDocument, string, string, string, string, string) (*models.FlowDocument, error) {
		return nil, fmt.Errorf("disk full")
	})
	decide(svc, true, nil)
	_, _ = svc.newFixApplier(doc, "stream-1", applierEnvelopes(rec), nil, nil).ApplyFixWithApproval(ctx, fixTestProposal())

	// a batch: one applied, one deselected
	three := threeCallDoc("flow-1")
	svc, _ = newFixTestService(three, func(_ context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		return withoutBlocks(d, "p-", blockID), nil
	})
	decide(svc, true, []int{1})
	props := []ai.FixProposal{proposalFor(t, svc, three, "p1", "b1"), proposalFor(t, svc, three, "p2", "b2")}
	_, _ = svc.newFixApplier(three, "stream-1", applierEnvelopes(rec), nil, nil).ApplyFixesWithApproval(ctx, props)
}

// goldenJSON renders v as indented JSON with the values that change from run to run fixed: stream and proposal ids
// (by order of appearance), durations.
func goldenJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var tree interface{}
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	var walk func(x interface{}) interface{}
	walk = func(x interface{}) interface{} {
		switch n := x.(type) {
		case map[string]interface{}:
			for k, val := range n {
				switch k {
				case "streamId":
					n[k] = "stream-1"
				case "proposalId":
					s, _ := val.(string)
					if _, ok := ids[s]; !ok {
						ids[s] = fmt.Sprintf("proposal-%d", len(ids)+1)
					}
					n[k] = ids[s]
				case "durationMs":
					n[k] = 1
				default:
					n[k] = walk(val)
				}
			}
		case []interface{}:
			for i := range n {
				n[i] = walk(n[i])
			}
		}
		return x
	}
	out, err := json.MarshalIndent(walk(tree), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}
