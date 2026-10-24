package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/ai/scrubber"
	"pad-core/models"
)

// The chat's egress net (R-testing-c15). Scrubbing is applied where each string is built — the document
// clone, the context texts, the history memo, every tool result, the approval card, the failure copy — and
// the provider boundary does not scrub again, so a path that forgets ScrubText, or a recall gap of the
// scrubber itself, leaks without a test noticing: the single-site tests check the site they know. This file
// plants one canary per channel in a flow parsed from real PAD source, runs a plain turn and both tool loops
// through StreamChatMessage against a capturing provider, and asserts that no canary reaches a provider
// request, the embedding provider or a stream event. A subtest per channel and per loop keeps a leaking
// channel readable on its own while the others stay green.
//
// Each canary is a secret by the scrubber's own rules where a scrub stands between it and the provider (an
// opaque 40-character token, a URL credential, a `password:` value), and a PAD literal where none does yet
// (an IF or LOOP header, a comment, a pasted SET line — the channels R-security-c13 and R-security-c14
// closed by masking Block.Name and giving free text PAD's literal rules). None is itself a shape the text scrubber masks, so a
// canary that reaches the provider got there because no pass looked at its context.
const (
	egressCanaryUnknownProp = "Vb7nQ2xK9pL4sW8rT3mZ6cY1hD5gF0jA2eU4iO7q"
	egressCanaryURLPassword = "Kp3mXv9qZt2rLw8n"
	egressCanaryURLSig      = "Hs7dQ2nV4bX9cM1k"
	egressCanaryFindingURL  = "Fn4kTq8wRz2vLm7y"
	egressCanaryIFLiteral   = "Pr0d!Pass-x91f"
	egressCanaryComment     = "Zq8vN2pL5xR9tW3kY7mB4cF6hJ1dG0sA2eU5iO8p"
	egressCanaryLoopLiteral = "Lw5tRq8nZv2kXm7yPb3dHc9sJf4gT6aQ1eU0iO2p"
	egressCanarySourceFile  = "Mx4bVq7nRt2kZw9yLp3dJc8sHf5gBa1eQu6iOy0r"
	egressCanaryUserMessage = "Winter#2024-u7Qx"
	egressCanaryHistory     = "Spring#2023-h3Lz"
	egressCanaryGuideline   = "Rg7tKq2mXz9vLw4nPb8dHc3s"
	egressCanaryProviderErr = "Gk8sPq2mVx7nLw3rTz9yBc4dHf6jMa1e"
)

// egressFlowSrc is the fixture flow. In order: a comment carrying an opaque token; a URL with userinfo and a
// SAS signature on an action the scrubber knows; a URL with a credential query parameter that the
// hardcoded-url rule copies into its finding; a property of an action the scrubber has no entry for, whose
// value only the entropy fallback can catch; an IF whose literal is held in Block.Name alone (the `%` closing
// the variable keeps the regex pass from matching); a LOOP WHILE whose literal is likewise in its header and
// whose finding carries the one auto-fix the chat may apply.
const egressFlowSrc = "#Region \"Main\"\n" +
	"    # Ops handover, vault reference " + egressCanaryComment + "\n" +
	"    WebAutomation.LaunchEdge Url: $'''https://svc:" + egressCanaryURLPassword + "@portal.example.com/in?sv=2022-11-02&sig=" + egressCanaryURLSig + "''' WindowState: WebAutomation.BrowserWindowState.Normal BrowserInstance=> Browser\n" +
	"    HTTPClient.InvokeUrl Url: $'''https://f.example.com/x?code=" + egressCanaryFindingURL + "''' Method: HTTPClient.Method.GET Response=> Resp\n" +
	"    Custom.Vault Note: $'''" + egressCanaryUnknownProp + "'''\n" +
	"    IF %DbPassword% = $'''" + egressCanaryIFLiteral + "''' THEN\n" +
	"        Display.ShowMessageDialog Title: $'''Hi''' Message: $'''ok'''\n" +
	"    END\n" +
	"    LOOP WHILE (%VaultToken%) <> ($'''" + egressCanaryLoopLiteral + "''')\n" +
	"        Display.ShowMessageDialog Title: $'''Retry''' Message: $'''again'''\n" +
	"    END\n" +
	"#EndRegion\n"

// egressHelpersFile is the selected source file: a high-entropy value assigned to a variable whose name is
// not a credential's, which the document scrub's entropy fallback masks and the source-text scrub does not.
const egressHelpersFile = "Helpers.txt"

const egressHelpersSrc = "#Region \"Helpers\"\n" +
	"    SET ServiceKey TO $'''" + egressCanarySourceFile + "'''\n" +
	"#EndRegion\n"

// The turn's free text: a pasted SET line in the question and another in the history, as PAD users paste
// flow code, and a guideline chunk whose `password:` value the text scrubber masks.
const (
	egressUserMessage = "Why does the login step fail? The flow sets it like this:\nSET Password TO $'''" + egressCanaryUserMessage + "'''"
	egressGuideline   = "Service accounts rotate quarterly; the staging password: " + egressCanaryGuideline + " is issued by Ops."
)

func egressHistory() []models.ChatMessage {
	return []models.ChatMessage{
		{ID: "h1", Role: "user", Content: "Earlier the flow had this line, is it a problem?\nSET Password TO $'''" + egressCanaryHistory + "'''"},
		{ID: "h2", Role: "assistant", Content: "A literal in a SET line is a hardcoded credential; store it in a secured input variable instead."},
	}
}

// errEgressProvider is the failure of the provider-error channel: a provider that echoes the request in its
// refusal, as a 400 with the offending field does.
var errEgressProvider = errors.New("upstream rejected the request: api_key=" + egressCanaryProviderErr + " is not valid for this deployment")

// The caller and the organisation the fixture flow belongs to: memberBackend (chat_rag_gate_test.go) makes
// user-1 a member of org-A, which the knowledge base's membership gate requires.
const (
	egressScope = "user-1"
	egressOrg   = "org-A"
)

// egressChannel is one way a secret can reach the provider: the canaries planted in it and the context block
// that puts the channel's content in front of the model.
type egressChannel struct {
	name     string
	canaries []string
	// block is the role of the turn's context block (egressFixture.blocks); "" selects the URL block, whose
	// own channels are closed, so the block adds nothing to the channel under test.
	block string
	// err is the provider's failure, for the provider-error channel.
	err error
}

var egressChannels = []egressChannel{
	{name: "unknown-action-property", canaries: []string{egressCanaryUnknownProp}, block: "unknown"},
	{name: "url-userinfo-and-sig", canaries: []string{egressCanaryURLPassword, egressCanaryURLSig}, block: "url"},
	{name: "if-literal", canaries: []string{egressCanaryIFLiteral}, block: "if"},
	{name: "comment-token", canaries: []string{egressCanaryComment}, block: "comment"},
	{name: "hardcoded-url-finding", canaries: []string{egressCanaryFindingURL}, block: "finding"},
	{name: "selected-source-file", canaries: []string{egressCanarySourceFile}},
	{name: "user-message", canaries: []string{egressCanaryUserMessage}},
	{name: "history-message", canaries: []string{egressCanaryHistory}},
	{name: "rag-guidelines", canaries: []string{egressCanaryGuideline}},
	// The LOOP header's literal: the selected block of the plain stream, the tool results of the loops, and the
	// approval card's label of the apply_fix.
	{name: "fix-preview", canaries: []string{egressCanaryLoopLiteral}, block: "loop"},
	{name: "provider-error", canaries: []string{egressCanaryProviderErr}, err: errEgressProvider},
}

// egressLoop is one of the three ways a turn runs: the plain stream, the native tool loop (a provider with
// function calling) and the prompt-marker loop (one without).
type egressLoop struct {
	name   string
	tools  bool // the request opts into tools
	native bool // the provider supports them natively
}

var egressLoops = []egressLoop{
	{name: "plain-stream"},
	{name: "tool-loop", tools: true, native: true},
	{name: "prompt-tool-loop", tools: true},
}

// egressFixture is one turn's worth of services over a fresh copy of the flow on disk: the apply_fix of the
// tool loops rewrites the file, so no two runs share one.
type egressFixture struct {
	svc      *ChatService
	notifier *testutil.CountingNotifier
	embedder *egressEmbedder
	doc      *models.FlowDocument
	report   *models.AnalysisReport
	// blocks are the fixture's block ids by role: comment, url, finding, unknown, if, loop.
	blocks  map[string]string
	subflow string
}

// egressEmbedder is the knowledge base's embedding provider: the query it embeds is egress too, so it records
// every text it is handed, and it answers one fixed vector, which the fixture's chunk shares.
type egressEmbedder struct {
	testutil.FakeProvider
	mu    sync.Mutex
	texts []string
}

func egressVector() []float32 { return []float32{1, 0, 0} }

func (e *egressEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.texts = append(e.texts, texts...)
	e.mu.Unlock()
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = egressVector()
	}
	return out, nil
}

func (e *egressEmbedder) inputs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.texts...)
}

func newEgressFixture(t *testing.T) *egressFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Main.txt")
	if err := os.WriteFile(path, []byte(egressFlowSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, egressHelpersFile), []byte(egressHelpersSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	notifier := &testutil.CountingNotifier{}
	settings := newTestSettingsStore(t)
	flows := NewFlowService(notifier, settings, NewLocalDocumentProvider(), nil, nil, nil)
	doc, err := flows.LoadFlowFromPath(path)
	if err != nil {
		t.Fatalf("load fixture flow: %v", err)
	}
	doc.OrganizationID = egressOrg // the knowledge base answers org flows only
	analysis, err := NewAnalysisService(notifier, settings, nil, AnalysisOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := analysis.AnalyzeFlowReadOnly(context.Background(), doc)
	if err != nil {
		t.Fatalf("analyze fixture flow: %v", err)
	}

	embedder := &egressEmbedder{}
	backend := &memberBackend{FakeBackend: testutil.FakeBackend{
		KnowledgeDocs:   map[string]*interfaces.KnowledgeDocument{"kd-1": {ID: "kd-1", OrgID: egressOrg, Filename: "guidelines.md"}},
		KnowledgeChunks: map[string]interfaces.KnowledgeChunk{"kc-1": {ID: "kc-1", DocID: "kd-1", OrgID: egressOrg, Content: egressGuideline, Embedding: egressVector()}},
	}}
	fx := &egressFixture{
		svc: &ChatService{
			notifier: notifier, configDir: t.TempDir(), flowCache: flows, analysisCache: analysis, settings: settings,
			factory: keyedFactory(), backend: backend, chatCtxCache: newChatContextCache(),
			knowledge: rag.NewKnowledgeService(backend, nil, nil, rag.WithEmbedder(embedder)),
		},
		notifier: notifier, embedder: embedder, doc: doc, report: report, blocks: map[string]string{},
	}

	roles := map[string]string{"COMMENT": "comment", "WebAutomation.LaunchEdge": "url", "HTTPClient.InvokeUrl": "finding", "Custom.Vault": "unknown", "IF": "if", "LOOP": "loop"}
	fx.subflow = doc.Subflows[0].ID
	for i := range doc.Subflows[0].Blocks {
		b := &doc.Subflows[0].Blocks[i]
		if role, ok := roles[b.RawType]; ok {
			fx.blocks[role] = b.ID
		}
	}
	if len(fx.blocks) != len(roles) {
		t.Fatalf("fixture parsed into blocks %v, want one of each role %v", fx.blocks, roles)
	}
	// The channels the analysis feeds must exist, or their subtests would pass for want of a finding.
	for _, want := range []struct{ rule, role string }{{"hardcoded-url", "url"}, {"hardcoded-url", "finding"}, {"infinite-loop-risk", "loop"}} {
		if !egressHasFinding(report, want.rule, fx.blocks[want.role]) {
			t.Fatalf("fixture analysis has no %s finding on the %s block; findings: %s", want.rule, want.role, egressFindingList(report))
		}
	}
	return fx
}

func egressHasFinding(report *models.AnalysisReport, rule, blockID string) bool {
	for _, f := range report.Findings {
		if f.RuleID == rule && f.BlockID == blockID {
			return true
		}
	}
	return false
}

func egressFindingList(report *models.AnalysisReport) string {
	var rules []string
	for _, f := range report.Findings {
		rules = append(rules, f.RuleID)
	}
	return strings.Join(rules, ", ")
}

// readToolCalls are the read tools of one model turn: every read tool in the registry, aimed at the blocks
// that carry canaries (the queries never carry one). Executed in one turn, so the apply_fix's refund keeps
// the run inside the step limit.
func (fx *egressFixture) readToolCalls() []ai.ToolCall {
	call := func(id, name string, input map[string]any) ai.ToolCall {
		b, err := json.Marshal(input)
		if err != nil {
			panic(err)
		}
		return ai.ToolCall{ID: id, Name: name, Input: b}
	}
	calls := []ai.ToolCall{
		call("s1", "search_flow", map[string]any{"query": "DbPassword"}),
		call("s2", "search_flow", map[string]any{"query": "handover"}),
	}
	for i, role := range []string{"comment", "url", "finding", "unknown", "if", "loop"} {
		calls = append(calls, call(fmt.Sprintf("g%d", i+1), "get_block", map[string]any{"blockId": fx.blocks[role]}))
	}
	return append(calls,
		call("sf", "get_subflow_blocks", map[string]any{"subflowId": fx.subflow}),
		call("lf", "list_findings", map[string]any{}),
		call("vl", "get_variable_lineage", map[string]any{"name": "DbPassword"}),
	)
}

// fixInput targets the LOOP's infinite-loop-risk finding: insert-exit-condition is a fix the chat may apply
// (not behaviour-changing), and its approval card names the block by its header.
func (fx *egressFixture) fixInput() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"blockId":%q,"ruleId":"infinite-loop-risk"}`, fx.blocks["loop"]))
}

// turns scripts the model: the plain stream answers at once; the loops read everything in one turn, preview
// the fix, apply it (approved by approveAll) and then answer.
func (l egressLoop) turns(fx *egressFixture) []testutil.FakeTurn {
	answer := testutil.FakeTurn{Text: "ok", TokensIn: 1, TokensOut: 1}
	if !l.tools {
		return []testutil.FakeTurn{answer}
	}
	reads := fx.readToolCalls()
	propose := ai.ToolCall{ID: "p1", Name: "propose_fix", Input: fx.fixInput()}
	apply := ai.ToolCall{ID: "a1", Name: "apply_fix", Input: fx.fixInput()}
	if l.native {
		return []testutil.FakeTurn{
			{ToolCalls: reads, TokensIn: 1, TokensOut: 1},
			{ToolCalls: []ai.ToolCall{propose}, TokensIn: 1, TokensOut: 1},
			{ToolCalls: []ai.ToolCall{apply}, TokensIn: 1, TokensOut: 1},
			answer,
		}
	}
	markers := func(calls ...ai.ToolCall) testutil.FakeTurn {
		var sb strings.Builder
		for _, c := range calls {
			sb.WriteString(markerCall(c.Name, string(c.Input)))
			sb.WriteString("\n")
		}
		return testutil.FakeTurn{Text: sb.String(), TokensIn: 1, TokensOut: 1}
	}
	return []testutil.FakeTurn{markers(reads...), markers(propose), markers(apply), answer}
}

// egressProviderID is the registered provider of the runs; its constructor hands out whichever provider the
// current run installed (the runs are sequential), so one registration serves them all.
const egressProviderID = "egress-canary"

var egressProvider struct {
	once sync.Once
	mu   sync.Mutex
	p    ai.Provider
}

func useEgressProvider(p ai.Provider) {
	egressProvider.once.Do(func() {
		ai.RegisterProvider(egressProviderID, func(string) ai.Provider {
			egressProvider.mu.Lock()
			defer egressProvider.mu.Unlock()
			return egressProvider.p
		})
	})
	egressProvider.mu.Lock()
	egressProvider.p = p
	egressProvider.mu.Unlock()
}

// egressEvent is one stream event as the client receives it: its type and its data as JSON.
type egressEvent struct {
	typ  string
	data string
}

// egressRun is everything one turn let out: the provider requests, the texts handed to the embedding
// provider and the stream's events.
type egressRun struct {
	fx       *egressFixture
	requests []ai.Request
	embeds   []string
	events   []egressEvent
}

// runEgressTurn runs one turn of loop with the channel's context block and failure, and returns what left.
func runEgressTurn(t *testing.T, loop egressLoop, ch egressChannel) *egressRun {
	t.Helper()
	fx := newEgressFixture(t)
	provider := &testutil.FakeProvider{Tools: loop.native, Turns: loop.turns(fx), Err: ch.err, RequireScrubbed: t}
	useEgressProvider(provider)

	block := ch.block
	if block == "" {
		block = "url"
	}
	streamID := uuid.NewString()
	req := models.ChatRequest{
		Provider: egressProviderID, Model: "fake-model",
		UserMessage: egressUserMessage, Messages: egressHistory(),
		ContextBlockID: fx.blocks[block], SelectedSourceFiles: []string{egressHelpersFile},
		UseTools: loop.tools, ClientStreamID: streamID,
	}
	stop := make(chan struct{})
	defer close(stop)
	go approveAll(fx.svc, streamID, stop)
	if _, err := fx.svc.StreamChatMessage(context.Background(), egressScope, fx.doc, fx.report, req); err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	waitStreamFinished(t, fx.svc, streamID)

	run := &egressRun{fx: fx, requests: provider.Requests(), embeds: fx.embedder.inputs()}
	for _, e := range fx.notifier.Events() {
		m, _ := e.Data.(map[string]interface{})
		if e.Name != "chat:event" || m["streamId"] != streamID {
			continue
		}
		typ, _ := m["type"].(string)
		b, err := json.Marshal(m["data"])
		if err != nil {
			t.Fatalf("event %s is not JSON: %v", typ, err)
		}
		run.events = append(run.events, egressEvent{typ: typ, data: string(b)})
	}
	run.requireCompleted(t, loop, ch)
	return run
}

// requireCompleted stops a run whose turn did not do what the channel needs: a leak cannot show in a turn that
// failed before the channel's content was built, and a fix that was never applied exercises no card.
func (r *egressRun) requireCompleted(t *testing.T, loop egressLoop, ch egressChannel) {
	t.Helper()
	if len(r.events) == 0 {
		t.Fatal("the turn emitted no event")
	}
	last := r.events[len(r.events)-1]
	if ch.err != nil {
		if last.typ != "error" {
			t.Fatalf("the provider failure did not end the turn with an error event; events: %v", r.types())
		}
		return
	}
	if last.typ != "done" {
		t.Fatalf("the turn did not finish: %s %s", last.typ, last.data)
	}
	if !loop.tools {
		return
	}
	if got, want := r.count("tool_result"), len(r.fx.readToolCalls())+2; got != want {
		t.Fatalf("tool results = %d, want %d (every read tool, propose_fix and apply_fix); events: %v", got, want, r.types())
	}
	applied := false
	for _, e := range r.events {
		if e.typ == "fix_decision" && strings.Contains(e.data, `"status":"applied"`) {
			applied = true
		}
	}
	if !applied {
		t.Fatalf("apply_fix was not applied and verified; events: %v", r.events)
	}
}

func (r *egressRun) types() []string {
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.typ
	}
	return out
}

func (r *egressRun) count(typ string) int {
	n := 0
	for _, e := range r.events {
		if e.typ == typ {
			n++
		}
	}
	return n
}

// leaks lists every place canary appears in what the run let out, each with the text around it.
func (r *egressRun) leaks(canary string) []string {
	var out []string
	for i, req := range r.requests {
		for _, s := range testutil.RequestStrings(req) {
			if strings.Contains(s.Text, canary) {
				out = append(out, fmt.Sprintf("request %d %s: %s", i+1, s.Path, egressExcerpt(s.Text, canary)))
			}
		}
	}
	for i, text := range r.embeds {
		if strings.Contains(text, canary) {
			out = append(out, fmt.Sprintf("embedding input %d: %s", i+1, egressExcerpt(text, canary)))
		}
	}
	for i, e := range r.events {
		if strings.Contains(e.data, canary) {
			out = append(out, fmt.Sprintf("event %d %s: %s", i+1, e.typ, egressExcerpt(e.data, canary)))
		}
	}
	return out
}

func egressExcerpt(text, canary string) string {
	i := strings.Index(text, canary)
	return fmt.Sprintf("%q", text[max(i-60, 0):min(i+len(canary)+40, len(text))])
}

// requireAbsent fails with every place a canary of the channel reached.
func (r *egressRun) requireAbsent(t *testing.T, ch egressChannel) {
	t.Helper()
	for _, canary := range ch.canaries {
		places := r.leaks(canary)
		if len(places) == 0 {
			continue
		}
		const shown = 8
		more := ""
		if len(places) > shown {
			more = fmt.Sprintf("\n  (+%d more)", len(places)-shown)
			places = places[:shown]
		}
		t.Errorf("canary %q of channel %s reached the provider or the client:\n  %s%s", canary, ch.name, strings.Join(places, "\n  "), more)
	}
}

// TestChatEgress_NoCanaryReachesTheProvider is the net: one turn per loop and channel, with the channel's
// block in context, and nothing of the channel's canary in any request, embedding input or event.
func TestChatEgress_NoCanaryReachesTheProvider(t *testing.T) {
	for _, loop := range egressLoops {
		for _, ch := range egressChannels {
			t.Run(loop.name+"/"+ch.name, func(t *testing.T) {
				run := runEgressTurn(t, loop, ch)
				run.requireAbsent(t, ch)
			})
		}
	}
}

// toolResultMarker is one result of the prompt-marker loop, fed back inside a user message.
var toolResultMarker = regexp.MustCompile(`(?s)<tool_result name="([^"]+)">\n(.*?)\n</tool_result>`)

// toolResults are tool's results as the provider received them (the last request holds every one) and as
// the client saw them: the tool_result row, and for apply_fix the approval card and its decision.
func (r *egressRun) toolResults(tool string) []string {
	var out []string
	if len(r.requests) > 0 {
		names := map[string]string{}
		for _, m := range r.requests[len(r.requests)-1].Messages {
			for _, tc := range m.ToolCalls {
				names[tc.ID] = tc.Name
			}
			if m.Role == "tool" && names[m.ToolCallID] == tool {
				out = append(out, m.Content)
			}
			if m.Role == "user" {
				for _, mm := range toolResultMarker.FindAllStringSubmatch(m.Content, -1) {
					if mm[1] == tool {
						out = append(out, mm[2])
					}
				}
			}
		}
	}
	for _, e := range r.events {
		switch e.typ {
		case "tool_result":
			if strings.Contains(e.data, `"name":"`+tool+`"`) {
				out = append(out, e.data)
			}
		case "fix_proposal":
			if strings.Contains(e.data, `"tool":"`+tool+`"`) {
				out = append(out, e.data)
			}
		case "fix_decision":
			if tool == "apply_fix" {
				out = append(out, e.data)
			}
		}
	}
	return out
}

// egressCanaries pairs every canary with the channel it was planted in, for messages.
func egressCanaries() map[string]string {
	out := map[string]string{}
	for _, ch := range egressChannels {
		for _, c := range ch.canaries {
			out[c] = ch.name
		}
	}
	return out
}

// TestChatEgress_ToolResultsCarryNoCanary looks at the tools one by one: every read tool of the registry, the
// fix preview and the approval-gated apply, in both loops, carry none of the fixture's canaries in their
// result or in the rows and cards the client gets for them. A read tool added to the registry fails here
// until the script exercises it.
func TestChatEgress_ToolResultsCarryNoCanary(t *testing.T) {
	for _, loop := range egressLoops {
		if !loop.tools {
			continue
		}
		run := runEgressTurn(t, loop, egressChannel{name: "tools"})
		exercised := map[string]bool{"propose_fix": true, "apply_fix": true}
		var tools []string
		for _, c := range run.fx.readToolCalls() {
			if !exercised[c.Name] {
				exercised[c.Name] = true
				tools = append(tools, c.Name)
			}
		}
		tools = append(tools, "propose_fix", "apply_fix")
		for _, spec := range ai.ToolSpecs() {
			if !exercised[spec.Def.Name] && !spec.RequiresApproval {
				t.Errorf("read tool %s is not exercised by the egress test: add a call for it to readToolCalls", spec.Def.Name)
			}
		}
		for _, tool := range tools {
			t.Run(loop.name+"/"+tool, func(t *testing.T) {
				results := run.toolResults(tool)
				if len(results) == 0 {
					t.Fatalf("%s left no result in the run", tool)
				}
				for canary, channel := range egressCanaries() {
					for _, res := range results {
						if strings.Contains(res, canary) {
							t.Errorf("the %s result carries canary %q of channel %s: %s", tool, canary, channel, egressExcerpt(res, canary))
						}
					}
				}
			})
		}
	}
}

// TestChatEgress_CanariesAreSound keeps the canaries meaningful: distinct, none inside another (a leak must
// name its channel), and none a shape the text scrubber masks on its own — the channels test what surrounds
// the secret, not the token.
func TestChatEgress_CanariesAreSound(t *testing.T) {
	canaries := egressCanaries()
	for a, chA := range canaries {
		if scrubber.ScrubText(a) != a {
			t.Errorf("canary %q of %s is masked on its own; its channel would pass without a scrub of its context", a, chA)
		}
		for b, chB := range canaries {
			if a != b && strings.Contains(a, b) {
				t.Errorf("canary %q of %s contains canary %q of %s", a, chA, b, chB)
			}
		}
	}
	if len(canaries) != 12 {
		t.Errorf("%d distinct canaries, want 12 (one per channel, two for the URL)", len(canaries))
	}
}
