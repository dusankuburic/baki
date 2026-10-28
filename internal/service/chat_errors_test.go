package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// --- failure classification ---------------------------------------------------

func TestClassifyFailure_Codes(t *testing.T) {
	ctl := &streamCtl{}
	cases := []struct {
		name     string
		err      error
		wantCode string
		wantMsg  string
	}{
		{"provider down (wrapped)", fmt.Errorf("claude stream error: %w", ai.ErrProviderDown), ChatErrProviderUnavailable, "temporarily unavailable"},
		{"circuit open", ai.ErrCircuitOpen, ChatErrProviderUnavailable, "temporarily unavailable"},
		{"rate limited", ai.ErrRateLimited, ChatErrProviderRateLimited, "rate-limiting"},
		{"bad key", ai.ErrApiKeyInvalid, ChatErrProviderAuth, "Settings"},
		// Finding #23: the credentials work, a role is missing — checked
		// before ErrApiKeyInvalid, which the error also matches.
		{"missing role", fmt.Errorf("azure ai foundry: %w: %w: the principal lacks the required data action", ai.ErrProviderPermissionDenied, ai.ErrApiKeyInvalid), ChatErrProviderPermission, "Foundry User role"},
		// Finding #15: Entra ID refused the service principal — no API key involved.
		{"entra credentials rejected", fmt.Errorf("get azure ai foundry token: %w: %w", ai.ErrEntraCredentialsRejected, ai.ErrApiKeyInvalid), ChatErrProviderAuth, "Microsoft Entra ID rejected"},
		// The token WAS issued and the resource refused it (also
		// ErrApiKeyInvalid): the copy names the access token, not a key the
		// principal does not have.
		{"entra token rejected", fmt.Errorf("%w: %w", ai.ErrEntraTokenRejected, ai.ErrApiKeyInvalid), ChatErrProviderAuth, "Entra ID access token"},
		// The identity service did not answer: unavailable, not an auth failure.
		{"entra token unavailable", fmt.Errorf("get azure ai foundry token: %w", ai.ErrEntraTokenUnavailable), ChatErrProviderUnavailable, "Entra ID token"},
		{"no balance", ai.ErrInsufficientBalance, ChatErrProviderBalance, "insufficient balance"},
		{"context limit", ai.ErrContextLimit, ChatErrContextLimit, "context window"},
		{"truncated", errors.New("openai stream truncated before terminal marker"), ChatErrInterrupted, "interrupted"},
		{"no key", fmt.Errorf("claude: %w", ai.ErrKeyNotConfigured), ChatErrProviderNotConfigured, "Settings"},
		{"provider setup", errProviderSetup, ChatErrProviderUnavailable, "provider configuration"},
		{"unknown", errors.New("something novel"), "", "something novel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg := ctl.classifyFailure(context.Background(), tc.err)
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			if !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("message = %q, want substring %q", msg, tc.wantMsg)
			}
		})
	}
}

// A deliberate stop surfaces its reason (and the STOPPED code) whatever error
// the cancelled call happened to return.
func TestClassifyFailure_StopWinsOverTheResultingError(t *testing.T) {
	ctl := &streamCtl{cancelReason: "response stopped"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, msg := ctl.classifyFailure(ctx, fmt.Errorf("provider call: %w", context.Canceled))
	if code != ChatErrStopped || msg != "response stopped" {
		t.Errorf("got (%q, %q), want the stop reason", code, msg)
	}
}

// --- in-stream provider errors (tool loop) ------------------------------------

// chunkErrProvider fails INSIDE the stream: an error chunk, then a clean return
// — how Claude reports "overloaded".
type chunkErrProvider struct {
	testutil.FakeProvider
	err error
}

func (p *chunkErrProvider) Stream(_ context.Context, _ ai.Request, onChunk func(ai.Chunk)) error {
	onChunk(ai.Chunk{Error: p.err})
	return nil
}

type capturedEvent struct {
	typ  string
	data map[string]interface{}
}

func captureEvents() (func(string, map[string]interface{}), func() []capturedEvent) {
	var mu sync.Mutex
	var evs []capturedEvent
	emit := func(typ string, data map[string]interface{}) {
		mu.Lock()
		evs = append(evs, capturedEvent{typ, data})
		mu.Unlock()
	}
	return emit, func() []capturedEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedEvent(nil), evs...)
	}
}

func TestRunToolLoop_InStreamProviderErrorIsFriendlyAndCoded(t *testing.T) {
	stub := &chunkErrProvider{err: fmt.Errorf("claude stream error: Overloaded: %w", ai.ErrProviderDown)}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, events := captureEvents()

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "hi"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	evs := events()
	if len(evs) == 0 || evs[len(evs)-1].typ != "error" {
		t.Fatalf("expected a final error event, got %+v", evs)
	}
	data := evs[len(evs)-1].data
	if data["code"] != ChatErrProviderUnavailable {
		t.Errorf("code = %v, want %s", data["code"], ChatErrProviderUnavailable)
	}
	msg, _ := data["message"].(string)
	if strings.Contains(msg, "Overloaded") || !strings.Contains(msg, "temporarily unavailable") {
		t.Errorf("message = %q, want the friendly copy, not the raw provider string", msg)
	}
	if ctl.errCode != ChatErrProviderUnavailable {
		t.Errorf("resume state lost the code: %q", ctl.errCode)
	}
}

// reportSink captures what the tool loop forwards to the exception sink for
// this test's scope (the sink is process-global; other tests may report too).
type reportSink struct {
	mu   sync.Mutex
	errs []error
}

func (r *reportSink) CaptureError(_ context.Context, err error, attrs errreport.Attrs) {
	if attrs["scope"] != "user-1" {
		return
	}
	r.mu.Lock()
	r.errs = append(r.errs, err)
	r.mu.Unlock()
}

func (r *reportSink) CapturePanic(context.Context, any, []byte, errreport.Attrs) {}

func (r *reportSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.errs)
}

func installReportSink(t *testing.T) *reportSink {
	t.Helper()
	sink := &reportSink{}
	errreport.Register(sink)
	t.Cleanup(func() { errreport.Register(nil) })
	return sink
}

// Stopped before the loop's first provider call: that is a stop, reported as one, and not something to page an
// operator about.
func TestRunToolLoop_StoppedBeforeTheFirstCallReportsTheStop(t *testing.T) {
	sink := installReportSink(t)
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{cancelReason: "response stopped"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	emit, events := captureEvents()

	svc.runToolLoop(ctx, &testutil.FakeProvider{HonourCtx: true}, ai.Request{}, "user-1",
		&ai.ToolContext{Ctx: ctx, Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	evs := events()
	if len(evs) != 1 || evs[0].typ != "error" {
		t.Fatalf("expected one error event, got %+v", evs)
	}
	if evs[0].data["code"] != ChatErrStopped || evs[0].data["message"] != "response stopped" {
		t.Errorf("got %+v, want the stop reason with code %s", evs[0].data, ChatErrStopped)
	}
	if n := sink.count(); n != 0 {
		t.Errorf("a user stop was forwarded to the exception sink %d time(s)", n)
	}
}

// --- provider setup -------------------------------------------------------------

// A missing key is the user's to fix; a key store that cannot be read is not,
// and its text ("keystore: … cipher: …") must not reach the chat.
func TestProviderSetupError_HidesKeystoreFailures(t *testing.T) {
	ctl := &streamCtl{}
	missing := providerSetupError(context.Background(), "user-1", "claude", fmt.Errorf("claude: %w", ai.ErrKeyNotConfigured))
	if code, msg := ctl.classifyFailure(context.Background(), missing); code != ChatErrProviderNotConfigured || !strings.Contains(msg, "Settings") {
		t.Errorf("missing key: got (%q, %q)", code, msg)
	}
	broken := providerSetupError(context.Background(), "user-1", "claude", fmt.Errorf("get claude key: keystore: open: cipher: message authentication failed"))
	code, msg := ctl.classifyFailure(context.Background(), broken)
	if code != ChatErrProviderUnavailable {
		t.Errorf("broken key store: code = %q, want %s", code, ChatErrProviderUnavailable)
	}
	if strings.Contains(msg, "keystore") || strings.Contains(msg, "cipher") {
		t.Errorf("broken key store: message leaks infrastructure text: %q", msg)
	}
}

// --- error code on resume / begin ---------------------------------------------

// A client that missed the live error event (reconnect, late begin) gets the
// code from the stored state too.
func TestResumeAndBegin_CarryTheErrorCode(t *testing.T) {
	svc := &ChatService{}
	ctl := &streamCtl{ownerID: "user-1", streamID: "s1", started: make(chan struct{})}
	ctl.setFailure(ChatErrProviderRateLimited, "the AI provider is rate-limiting requests — wait a moment and try again", false)
	svc.activeStreams.Store("s1", ctl)

	res, err := svc.ResumeStream(context.Background(), "s1", 0)
	if err != nil {
		t.Fatalf("ResumeStream: %v", err)
	}
	if res.ErrorCode != ChatErrProviderRateLimited || res.Error == "" {
		t.Errorf("resume: got (%q, %q), want the stored code and message", res.ErrorCode, res.Error)
	}
	begin := svc.BeginStream(context.Background(), "s1")
	if begin == nil || begin.ErrorCode != ChatErrProviderRateLimited {
		t.Errorf("begin: got %+v, want ErrorCode %s", begin, ChatErrProviderRateLimited)
	}
}

// --- heartbeat --------------------------------------------------------------------

// Keepalive timing for tests: a heartbeat after 80ms of silence, checked every
// 5ms. Wide enough that scheduler hiccups under -race don't fake a silence.
const (
	testHeartbeatAfter = 80 * time.Millisecond
	testHeartbeatTick  = 5 * time.Millisecond
)

// runHeartbeat starts heartbeatStream with the test timing. stop closes the
// stop channel (once) and waits for the goroutine — a test must join it before
// returning, or the goroutine outlives the test's captures. exited reports the
// goroutine ending on its own (terminal stream state).
func runHeartbeat(ctl *streamCtl, emit func(string, map[string]interface{})) (stop func(), exited <-chan struct{}) {
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		heartbeatStream(context.Background(), ctl, emit, stopCh, testHeartbeatAfter, testHeartbeatTick)
		close(done)
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stopCh) })
		<-done
	}, done
}

func TestHeartbeatTiming_DefaultsUnlessOverridden(t *testing.T) {
	if after, tick := (&ChatService{}).heartbeatTiming(); after != defaultHeartbeatAfter || tick != defaultHeartbeatTick {
		t.Errorf("defaults: got (%v, %v)", after, tick)
	}
	svc := &ChatService{heartbeatAfter: testHeartbeatAfter, heartbeatTick: testHeartbeatTick}
	if after, tick := svc.heartbeatTiming(); after != testHeartbeatAfter || tick != testHeartbeatTick {
		t.Errorf("overrides: got (%v, %v)", after, tick)
	}
}

func countType(evs []capturedEvent, typ string) int {
	n := 0
	for _, e := range evs {
		if e.typ == typ {
			n++
		}
	}
	return n
}

func TestHeartbeatStream_KeepsASilentStreamAlive(t *testing.T) {
	ctl := &streamCtl{started: make(chan struct{})}
	close(ctl.started)
	ctl.lastEmit.Store(time.Now().UnixNano())
	emit, events := captureEvents()
	stop, exited := runHeartbeat(ctl, emit)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for countType(events(), "heartbeat") == 0 && time.Now().Before(deadline) {
		time.Sleep(testHeartbeatTick)
	}
	if countType(events(), "heartbeat") == 0 {
		t.Fatal("no heartbeat during a silent phase")
	}
	// A heartbeat is for the CLIENT only: the provider-idle watchdog must still
	// see the provider as silent.
	if ctl.lastActivity.Load() != 0 {
		t.Error("heartbeat touched the provider-idle watchdog")
	}

	// A terminal state ends it on its own.
	ctl.mu.Lock()
	ctl.done = true
	ctl.mu.Unlock()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Error("heartbeat kept running after the stream finished")
	}
}

func TestHeartbeatStream_QuietWhileEventsFlowAndBeforeStart(t *testing.T) {
	// Not started: nobody is subscribed yet, so silence is expected.
	ctl := &streamCtl{started: make(chan struct{})}
	ctl.lastEmit.Store(time.Now().Add(-time.Hour).UnixNano())
	emit, events := captureEvents()
	stop, _ := runHeartbeat(ctl, emit)
	time.Sleep(3 * testHeartbeatAfter)
	stop()
	if n := countType(events(), "heartbeat"); n != 0 {
		t.Errorf("heartbeat emitted before the client subscribed (%d)", n)
	}

	// Started, with events flowing: the client is never silent for `after`.
	ctl2 := &streamCtl{started: make(chan struct{})}
	close(ctl2.started)
	ctl2.lastEmit.Store(time.Now().UnixNano()) // seeded BEFORE the first tick can look
	emit2, events2 := captureEvents()
	stop2, _ := runHeartbeat(ctl2, emit2)
	deadline := time.Now().Add(3 * testHeartbeatAfter)
	for time.Now().Before(deadline) {
		ctl2.lastEmit.Store(time.Now().UnixNano()) // live events keep flowing
		time.Sleep(time.Millisecond)
	}
	stop2()
	if n := countType(events2(), "heartbeat"); n != 0 {
		t.Errorf("heartbeat emitted while events were flowing (%d)", n)
	}
}

// done.chunks lets the client detect dropped chunks; a heartbeat must not count.
func TestHeartbeat_NotCountedAsAChunk(t *testing.T) {
	emit, _ := captureEvents()
	c := newChunkCoalescer(emit)
	wrapped := c.wrap()
	wrapped("heartbeat", map[string]interface{}{})
	wrapped("chunk", map[string]interface{}{"content": "x"})
	wrapped("heartbeat", map[string]interface{}{})
	if n := c.flushAndCount(); n != 1 {
		t.Errorf("chunk count = %d, want 1", n)
	}
}

// --- client message ids -------------------------------------------------------------

func TestPersistedTurns_UseTheClientMessageIDs(t *testing.T) {
	svc := &ChatService{configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}
	req := models.ChatRequest{
		ConversationID:     "11111111-1111-4111-8111-111111111111",
		UserMessage:        "hi",
		UserMessageID:      "22222222-2222-4222-8222-222222222222",
		AssistantMessageID: "33333333-3333-4333-8333-333333333333",
	}
	ctx := context.Background()

	svc.persistInitialUserTurn(ctx, doc, req)
	ctl := &streamCtl{done: true}
	ctl.buffer.WriteString("the answer")
	svc.persistAssistantTurn(ctx, doc, req, ctl, req.Model, "")
	// Idempotent: a second pass (retry) must not append again.
	svc.persistAssistantTurn(ctx, doc, req, ctl, req.Model, "")

	got, err := svc.GetConversation(ctx, doc.ID, req.ConversationKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("stored %d messages, want 2: %+v", len(got), got)
	}
	if got[0].ID != req.UserMessageID || got[1].ID != req.AssistantMessageID {
		t.Errorf("stored ids (%s, %s), want the client's (%s, %s)", got[0].ID, got[1].ID, req.UserMessageID, req.AssistantMessageID)
	}
}

// Stop pressed just as the stream finished: the client saved its interrupted
// partial under the turn's id first. The server's full answer must not be
// appended after it — the reply would show twice.
func TestPersistAssistantTurn_RespectsTheClientsCopyOfTheTurn(t *testing.T) {
	svc := &ChatService{configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}
	req := models.ChatRequest{
		ConversationID:     "11111111-1111-4111-8111-111111111111",
		AssistantMessageID: "33333333-3333-4333-8333-333333333333",
	}
	ctx := context.Background()
	if err := svc.SaveConversation(ctx, doc.ID, req.ConversationKey(), []models.ChatMessage{
		{ID: "22222222-2222-4222-8222-222222222222", Role: "user", Content: "hi", Timestamp: time.Now()},
		{ID: req.AssistantMessageID, Role: "assistant", Content: "the ans", Timestamp: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	ctl := &streamCtl{done: true}
	ctl.buffer.WriteString("the answer, in full")
	svc.persistAssistantTurn(ctx, doc, req, ctl, req.Model, "")

	got, _ := svc.GetConversation(ctx, doc.ID, req.ConversationKey())
	if len(got) != 2 {
		t.Errorf("stored %d messages, want the client's 2", len(got))
	}
}

// The client already saved this turn's answer under its id AND asked its next
// question: the deferred server persist must not append the answer again
// (after the new question — the reply would show twice, out of order).
func TestPersistAssistantTurn_DedupesByIDAnywhereInTheHistory(t *testing.T) {
	svc := &ChatService{configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}
	req := models.ChatRequest{
		ConversationID:     "11111111-1111-4111-8111-111111111111",
		AssistantMessageID: "33333333-3333-4333-8333-333333333333",
	}
	ctx := context.Background()
	if err := svc.SaveConversation(ctx, doc.ID, req.ConversationKey(), []models.ChatMessage{
		{ID: "22222222-2222-4222-8222-222222222222", Role: "user", Content: "hi", Timestamp: time.Now()},
		{ID: req.AssistantMessageID, Role: "assistant", Content: "the answer", Timestamp: time.Now()},
		{ID: "44444444-4444-4444-8444-444444444444", Role: "user", Content: "and then?", Timestamp: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	ctl := &streamCtl{done: true}
	ctl.buffer.WriteString("the answer")
	svc.persistAssistantTurn(ctx, doc, req, ctl, req.Model, "")

	got, _ := svc.GetConversation(ctx, doc.ID, req.ConversationKey())
	if len(got) != 3 {
		t.Errorf("stored %d messages, want the 3 already there: %+v", len(got), got)
	}
}

// A resend that reuses the previous turn's id must not store two messages
// under one id — the client dedupes by id.
func TestPersistInitialUserTurn_MintsAFreshIDWhenTheClientsIsTaken(t *testing.T) {
	svc := &ChatService{configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}
	taken := "22222222-2222-4222-8222-222222222222"
	req := models.ChatRequest{
		ConversationID: "11111111-1111-4111-8111-111111111111",
		UserMessage:    "again",
		UserMessageID:  taken,
		Messages:       []models.ChatMessage{{ID: taken, Role: "user", Content: "first", Timestamp: time.Now()}},
	}
	svc.persistInitialUserTurn(context.Background(), doc, req)

	got, _ := svc.GetConversation(context.Background(), doc.ID, req.ConversationKey())
	if len(got) != 2 || got[0].ID != taken || got[1].ID == taken || len(got[1].ID) != 36 {
		t.Errorf("stored ids %+v, want the taken id once and a fresh one for the new turn", got)
	}
}

func TestClientMessageID_OnlyAcceptsCanonicalUUIDs(t *testing.T) {
	if got := clientMessageID("22222222-2222-4222-8222-222222222222"); got != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("a valid client id was replaced: %q", got)
	}
	// Alternative spellings uuid.Parse accepts would not match the client's own
	// string comparison, so they are replaced too.
	for _, bad := range []string{
		"", "not-a-uuid", "../../etc",
		"{22222222-2222-4222-8222-222222222222}",
		"urn:uuid:22222222-2222-4222-8222-222222222222",
		"22222222222242228222222222222222",
	} {
		if got := clientMessageID(bad); got == bad || len(got) != 36 {
			t.Errorf("clientMessageID(%q) = %q, want a server-minted uuid", bad, got)
		}
	}
}

// ADR 0028: a provider the policy refused at the factory reaches the stream's error event with its own code
// and the policy's copy, and providerSetupError passes it through unlogged (the user's or the org admin's to
// act on, not an incident).
func TestClassifyFailure_ProviderNotAllowedHasCode(t *testing.T) {
	ctl := &streamCtl{}
	cause := fmt.Errorf("%w: xai is not on your organisation's list of allowed AI providers — pick one that is", ai.ErrProviderNotAllowed)
	if got := providerSetupError(context.Background(), "u1", "xai", cause); !errors.Is(got, cause) {
		t.Errorf("providerSetupError replaced the refusal with %v", got)
	}
	code, msg := ctl.classifyFailure(context.Background(), cause)
	if code != ChatErrProviderNotAllowed || ChatErrProviderNotAllowed != "PROVIDER_NOT_ALLOWED" {
		t.Errorf("code = %q, want PROVIDER_NOT_ALLOWED", code)
	}
	if !strings.Contains(msg, "xai is not on your organisation's list") {
		t.Errorf("message = %q, want the policy's copy", msg)
	}
	// Not a shared-credential class: the bubble must not send the member to Organizations for a credential.
	ctl.sharedCredential.Store(true)
	if _, _, shared := ctl.classifyFailureShared(context.Background(), cause); shared {
		t.Error("a policy refusal was flagged as a shared-credential failure")
	}
}
