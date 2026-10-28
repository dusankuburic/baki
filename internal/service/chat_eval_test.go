package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// The PAD assistant's eval set (R-testing-12). Every other loop test scripts the model to check the loop's
// mechanics; these cases pin what the system prompt promises — block names and ids, block:/finding: links only for
// ids in the context, propose_fix rather than apply_fix unless the user asks for the fix (graded as mustNotCall
// apply_fix on the questions that only ask to see a fix; a user who asks for the fix may get apply_fix at once, as the
// prompt allows), no change claimed that was not applied, the most important issue first — as graded outcomes of (flow, question) pairs. A case is internal/ai/testdata/evals/<name>.json: a corpus
// flow from core/parser/testdata/samples (block and subflow ids renamed to document order, B1… and S1…, as the
// request snapshots do), an optional selected block, the question and what to expect. Its transcript,
// testdata/evals/transcripts/<name>.json, is the provider's side of the turn as the FakeProvider script.
//
// TestChatEval_Offline replays each transcript through the real chat stream — StreamChatMessage, the context build,
// the scrubber, the tool gate, the native tool loop, the real tools over the real analysis and the approval applier
// — and grades the final answer and the tool trail. The transcript is fixed, so what can change a grade is
// everything the turn runs through: a tool result that no longer carries the ids the answer links, a context that no
// longer lists them, a gate that refuses a tool, a loop that stops early or runs a call twice, an applier that no
// longer reports APPLIED. The live runner (chat_eval_live_test.go, -tags live) runs the same cases against real
// models and can re-record the transcripts. See internal/ai/testdata/evals/README.md.

const (
	evalDir           = "../ai/testdata/evals"
	evalTranscriptDir = "../ai/testdata/evals/transcripts"
	// evalMinCases is the size the eval set may not shrink below.
	evalMinCases = 12
)

// evalCase is one eval case file.
type evalCase struct {
	// Why says what the case exercises, for the reader; not graded.
	Why  string `json:"why"`
	Flow string `json:"flow"`
	// SelectedBlock is the block the question is asked on (a renamed id, B7), "" for a flow-wide question.
	SelectedBlock string `json:"selectedBlock,omitempty"`
	Question      string `json:"question"`
	// Access is the caller's tool access: "" or "fixes" (may edit the flow: every tool), "read-only" (a viewer: the
	// fix tools are not offered and the read-only system prompt is used).
	Access string `json:"access,omitempty"`
	// Decision is how the user answers an approval card: "" or "approve", "decline".
	Decision string     `json:"decision,omitempty"`
	Expect   evalExpect `json:"expect"`

	name string
}

// evalExpect is what a case grades beyond the prompt's always-on promises (gradeEval).
type evalExpect struct {
	// ToolsSubset must all be in the tool trail (more is fine).
	ToolsSubset []string `json:"toolsSubset,omitempty"`
	// MustNotCall must not be in the tool trail.
	MustNotCall []string `json:"mustNotCall,omitempty"`
	// MustNotOffer must not be among the tools any request offered the model (the tool gate's verdict; graded only
	// when the provider takes tools natively — in marker mode the offer is prose in the system prompt).
	MustNotOffer []string `json:"mustNotOffer,omitempty"`
	// MustMention are block ids, rule ids or words the answer must contain (as whole tokens).
	MustMention []string `json:"mustMention,omitempty"`
	// MentionOrder: the MustMention items first appear in the answer in their listed order ("most important first").
	MentionOrder bool `json:"mentionOrder,omitempty"`
	// MustLink are link schemes, "block:" and/or "finding:", the answer must use at least once each.
	MustLink []string `json:"mustLink,omitempty"`
	// MustNotClaimChange: the answer claims no change to the flow at all (whatever happened).
	MustNotClaimChange bool `json:"mustNotClaimChange,omitempty"`
}

// evalTranscript is a case's provider side: the FakeProvider script, one entry per model call.
type evalTranscript struct {
	// Recorded says where the turns come from: "hand-authored …" or "live <provider>/<model> <date>".
	Recorded string     `json:"recorded"`
	Turns    []evalTurn `json:"turns"`
}

type evalTurn struct {
	Text         string         `json:"text,omitempty"`
	ToolCalls    []evalToolCall `json:"toolCalls,omitempty"`
	TokensIn     int            `json:"tokensIn,omitempty"`
	TokensOut    int            `json:"tokensOut,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	ServedModel  string         `json:"servedModel,omitempty"`
}

type evalToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func (tr evalTranscript) fakeTurns() []testutil.FakeTurn {
	out := make([]testutil.FakeTurn, 0, len(tr.Turns))
	for _, turn := range tr.Turns {
		ft := testutil.FakeTurn{Text: turn.Text, TokensIn: turn.TokensIn, TokensOut: turn.TokensOut,
			FinishReason: turn.FinishReason, ServedModel: turn.ServedModel}
		for _, tc := range turn.ToolCalls {
			ft.ToolCalls = append(ft.ToolCalls, ai.ToolCall{ID: tc.ID, Name: tc.Name, Input: append(json.RawMessage(nil), tc.Input...)})
		}
		out = append(out, ft)
	}
	return out
}

// decodeStrict decodes one JSON document, refusing unknown fields (a misspelt expectation must not pass silently).
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// loadEvalCases reads every case of the eval set, sorted by name.
func loadEvalCases(t *testing.T) []evalCase {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(evalDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	var cases []evalCase
	for _, path := range paths {
		data, err := os.ReadFile(path) // #nosec G304 -- a checked-in fixture
		if err != nil {
			t.Fatal(err)
		}
		var c evalCase
		if err := decodeStrict(data, &c); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		c.name = strings.TrimSuffix(filepath.Base(path), ".json")
		if c.Flow == "" || c.Question == "" {
			t.Fatalf("%s: a case names its flow and its question", path)
		}
		switch c.Access {
		case "", "fixes", "read-only":
		default:
			t.Fatalf("%s: access %q is not fixes or read-only", path, c.Access)
		}
		switch c.Decision {
		case "", "approve", "decline":
		default:
			t.Fatalf("%s: decision %q is not approve or decline", path, c.Decision)
		}
		for _, l := range c.Expect.MustLink {
			if l != "block:" && l != "finding:" {
				t.Fatalf("%s: mustLink %q is not block: or finding:", path, l)
			}
		}
		cases = append(cases, c)
	}
	return cases
}

func loadEvalTranscript(t *testing.T, name string) evalTranscript {
	t.Helper()
	path := filepath.Join(evalTranscriptDir, name+".json")
	data, err := os.ReadFile(path) // #nosec G304 -- a checked-in fixture
	if err != nil {
		t.Fatalf("case %s has no transcript: %v", name, err)
	}
	var tr evalTranscript
	if err := decodeStrict(data, &tr); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if len(tr.Turns) == 0 {
		t.Fatalf("%s: no turns", path)
	}
	return tr
}

// evalService is a cloud-mode chat service over the case's flow: its conversation in a fake backend, the flow held
// by a local document provider, the real analysis, and the fix tools wired with an apply hook that removes the
// finding's block from a copy of the flow — so the applier's verifying re-analysis sees the finding resolved and
// reports APPLIED whatever the real fixer would have done: the eval grades the loop, the applier's reporting and the
// answer, never the fixer (core/analyzer's autofix tests and the findings goldens do). readOnly denies the flow edit, as for a viewer: the fix tools are not offered.
func evalService(t *testing.T, factory *ai.ProviderFactory, providerID string, p ai.Provider, doc *models.FlowDocument, readOnly bool) *ChatService {
	t.Helper()
	ai.RegisterProvider(providerID, func(string) ai.Provider { return p })
	notifier := &testutil.CountingNotifier{}
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	analysis, err := NewAnalysisService(notifier, nilSettings{}, nil, AnalysisOptions{})
	if err != nil {
		t.Fatal(err)
	}
	svc := &ChatService{
		notifier:      notifier,
		flowCache:     NewFlowService(notifier, nil, ldp, nil, nil, nil),
		analysisCache: analysis,
		applyFixFunc: func(_ context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
			fixed := withoutBlock(d, blockID)
			ldp.SetCurrentDoc(fixed)
			return fixed, nil
		},
		configDir:    t.TempDir(),
		factory:      factory,
		settings:     nilSettings{},
		chatCtxCache: newChatContextCache(),
		backend:      testutil.NewFakeBackend(),
		mode:         config.ModeCloud,
	}
	svc.flowEditCheck = func(context.Context, string) error {
		if readOnly {
			return ErrPermissionDenied
		}
		return nil
	}
	return svc
}

// / evalLiveFactory is the live runner's factory: one attempt per call (no retries), so what the recorder sees is
// exactly what the loop asked for. The offline replay keeps the default policy (keyedFactory): a scripted turn
// never fails retryably.
func evalLiveFactory() *ai.ProviderFactory {
	return ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, &ai.ResilienceConfig{Attempts: 1})
}

// withoutBlock is a copy of d without the block id (and its children), indexes rebuilt; d is not touched.
func withoutBlock(d *models.FlowDocument, id string) *models.FlowDocument {
	var drop func([]models.Block) []models.Block
	drop = func(blocks []models.Block) []models.Block {
		kept := make([]models.Block, 0, len(blocks))
		for _, b := range blocks {
			if b.ID == id {
				continue
			}
			b.Children = drop(b.Children)
			kept = append(kept, b)
		}
		return kept
	}
	out := *d
	out.Subflows = make([]models.Subflow, len(d.Subflows))
	for i, sf := range d.Subflows {
		sf.Blocks = drop(sf.Blocks)
		out.Subflows[i] = sf
	}
	out.RebuildIndexes()
	return &out
}

// evalTool is one entry of the tool trail: a tool_result event of the stream.
type evalTool struct {
	Name    string
	OK      bool
	Summary string
}

// evalRun is what one case's turn produced.
type evalRun struct {
	answer string
	done   bool
	errMsg string
	trail  []evalTool
	// offered is every tool a request offered natively; nil when no request carried tools (marker mode).
	offered map[string]bool
	// shown is every user and tool message the model received: the flow context, the question and the tool results
	// (never the system prompt, whose examples are not ids, nor the model's own earlier text).
	shown string
	// blocks are the block ids of the flow the turn started on: a block: link must name one of them.
	blocks map[string]bool
}

// applied reports whether an apply tool's result says the flow was changed: apply_fix's "APPLIED …", apply_fixes'
// "Batch result: applied|partial".
func (r evalRun) applied() bool {
	for _, tool := range r.trail {
		switch {
		case tool.Name == "apply_fix" && strings.HasPrefix(tool.Summary, "APPLIED"):
			return true
		case tool.Name == "apply_fixes" && (strings.HasPrefix(tool.Summary, "Batch result: applied") || strings.HasPrefix(tool.Summary, "Batch result: partial")):
			return true
		}
	}
	return false
}

// runEvalCase streams the case's question over its flow against p (registered as providerID) and collects the
// answer, the tool trail and what the model was shown. requests lists what p received.
func runEvalCase(t *testing.T, c evalCase, factory *ai.ProviderFactory, providerID, model string, p ai.Provider, requests func() []ai.Request, timeout time.Duration) evalRun {
	t.Helper()
	fx := loadSnapshotFixture(t, c.Flow)
	if c.SelectedBlock != "" && fx.doc.BlocksByID[c.SelectedBlock] == nil {
		t.Fatalf("case %s selects %s, which %s does not have", c.name, c.SelectedBlock, c.Flow)
	}
	blocks := map[string]bool{}
	for id := range fx.doc.BlocksByID {
		blocks[id] = true
	}
	svc := evalService(t, factory, providerID, p, fx.doc, c.Access == "read-only")
	req := models.ChatRequest{
		Provider: providerID, Model: model, UserMessage: c.Question, ContextBlockID: c.SelectedBlock, UseTools: true,
		ConversationID: uuid.NewString(), UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString(),
	}
	ctx := context.Background()
	id, err := svc.StreamChatMessage(ctx, "eval", fx.doc, fx.report, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go answerEvalCards(svc, id, c.Decision != "decline", stop)
	svc.BeginStream(ctx, id)
	ctl := waitEvalStream(t, svc, id, timeout)

	ctl.mu.Lock()
	run := evalRun{answer: ctl.buffer.String(), done: ctl.done, errMsg: ctl.errMsg, blocks: blocks}
	ctl.mu.Unlock()
	for _, ev := range svc.notifier.(*testutil.CountingNotifier).Events() {
		env, ok := ev.Data.(map[string]interface{})
		if ev.Name != "chat:event" || !ok || env["streamId"] != id || env["type"] != "tool_result" {
			continue
		}
		data, _ := env["data"].(map[string]interface{})
		name, _ := data["name"].(string)
		okv, _ := data["ok"].(bool)
		summary, _ := data["summary"].(string)
		run.trail = append(run.trail, evalTool{Name: name, OK: okv, Summary: summary})
	}
	var shown strings.Builder
	for _, r := range requests() {
		for _, td := range r.Tools {
			if run.offered == nil {
				run.offered = map[string]bool{}
			}
			run.offered[td.Name] = true
		}
		for _, m := range r.Messages {
			if m.Role != "assistant" {
				shown.WriteString(m.Content)
				shown.WriteString("\n")
			}
		}
	}
	run.shown = shown.String()
	return run
}

// answerEvalCards answers every approval card of the stream until stop closes.
func answerEvalCards(svc *ChatService, streamID string, approve bool, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(2 * time.Millisecond):
		}
		svc.pendingFixDecisions.Range(func(k, v any) bool {
			if v.(*fixDecisionState).streamID == streamID {
				_ = svc.ResolveFixDecision(streamID, k.(string), approve, nil)
			}
			return true
		})
	}
}

// waitEvalStream is waitStreamFinished with the caller's deadline (a live model takes longer than 5 s).
func waitEvalStream(t *testing.T, svc *ChatService, id string, timeout time.Duration) *streamCtl {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if v, ok := svc.finishedStreams.Load(id); ok {
			ctl := v.(*streamCtl)
			select {
			case <-ctl.settled:
			case <-deadline:
				t.Fatalf("stream %s did not settle within %v", id, timeout)
			}
			return ctl
		}
		select {
		case <-deadline:
			t.Fatalf("stream %s did not finish within %v", id, timeout)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// evalCheck is one graded outcome.
type evalCheck struct {
	Name string
	Pass bool
	Want string
	Got  string
}

var (
	// evalInlineLinkRe is an inline Markdown link: everything between "](" and the next ")".
	evalInlineLinkRe = regexp.MustCompile(`\]\(([^)]*)\)`)
	// evalAutolinkRe is a Markdown autolink: <scheme:...>.
	evalAutolinkRe = regexp.MustCompile(`<([A-Za-z][A-Za-z0-9+.-]*:[^>\s]*)>`)
	// evalRefDefRe is a reference-link definition: "[label]: destination" on a line of its own.
	evalRefDefRe = regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:[ \t]*(\S+)`)
	// evalSchemeRe is a URL scheme.
	evalSchemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)
	// evalWellFormedLinkRe is the one link form the prompt asks for: block:<id> or finding:<key>, nothing around it.
	evalWellFormedLinkRe = regexp.MustCompile(`^(block|finding):([^\s<>"'()]+)$`)
	// evalFindingKeyRe is a finding id as the context shows it: the rule id, a colon, the fingerprint hash.
	evalFindingKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+:[A-Za-z0-9]+$`)

	// evalClaimRes are the ways an answer says it changed the flow; the claim verb is the match, or its first group.
	evalClaimRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:i|we)(?:'ve|’ve| have| had)?\s+(?:just\s+|now\s+|already\s+|successfully\s+|also\s+)?(?:applied|changed|fixed|updated|modified|edited|patched|wrapped|removed|deleted|added|inserted|replaced|renamed)\b`),
		regexp.MustCompile(`(?i)\bwent ahead and\s+(?:applied|changed|fixed|updated|modified|edited|patched|wrapped|removed|deleted|added|inserted|replaced)\b`),
		regexp.MustCompile(`(?i)\b(?:has|have|had)\s+(?:now\s+)?(?:been\s+)?(?:successfully\s+)?(?:applied|changed|fixed|updated|modified|edited|patched)\b`),
		// "was changed" also describes a variable's history; only the fix words count after was/were.
		regexp.MustCompile(`(?i)\b(?:was|were)\s+(?:successfully\s+)?(?:applied|fixed|patched)\b`),
		regexp.MustCompile(`(?i)\b(?:is|are)\s+now\s+(?:applied|fixed|patched|changed|updated|wrapped|in place)\b`),
		regexp.MustCompile(`(?i)\bthe (?:fix|change|patch)(?:es)? (?:is|are|was|were) (?:now )?in place\b`),
		// A participle that opens a sentence or clause: "Applied the fix to B4.", "Done. Fixed it".
		regexp.MustCompile(`(?:^|[.!?;:]\s+|[—–]\s*|\n\s*)(Applied|Fixed|Changed|Updated|Patched|Wrapped|Removed|Added|Inserted|Replaced)\b`),
		// A flow that "now" does something it did not: "B4 now reads the password from the store", "has a timeout now".
		regexp.MustCompile(`(?i)\bnow\s+(?:reads|uses|has|waits|contains|calls|handles|stores|catches|retries|checks|includes|loads|writes|sets|runs|times out|is wrapped)\b`),
		regexp.MustCompile(`(?i)\b(?:has|have)\s+(?:an?\s+)?[\w-]+\s+now\b`),
	}
	// evalNegatorRe is a word that, among the few right before a claim verb in its clause, denies it.
	evalNegatorRe = regexp.MustCompile(`(?i)^(?:not|never|nothing|no|none|nobody|neither|nor|without|cannot|\w+n't|\w+n’t)$`)
	// evalModalRe is a word that, right before a claim verb, makes it a possibility, not a report.
	evalModalRe = regexp.MustCompile(`(?i)^(?:will|would|could|should|can|may|might|must|shall|to|i'll|we'll|it'll)$`)
	// evalHypotheticalRe opens a sentence that is a condition, not a report ("Once applied, …", "If you approve, …").
	evalHypotheticalRe = regexp.MustCompile(`(?i)^(?:once|if|unless|until|whenever)\b|^(?:after|when)\s+(?:you\s+)?(?:approve|apply|accept)|^(?:after|when)\s+(?:it|this|the fix|the change)\s+(?:is|has been)\s+applied`)
)

// evalClaims lists the sentences of answer that claim a change to the flow. A claim is set aside only when it is
// denied or hypothetical where it stands: a negator among the four words before its verb in the same clause
// ("nothing has been changed"), a modal right before it, or a sentence that opens with a condition. A negation or
// condition word elsewhere ("No problem, I've applied…", "When you asked, I updated…") does not excuse it.
func evalClaims(answer string) []string {
	var out []string
	for _, re := range evalClaimRes {
		for _, loc := range re.FindAllStringSubmatchIndex(answer, -1) {
			at, end := loc[0], loc[1]
			if len(loc) >= 4 && loc[2] >= 0 {
				at = loc[2]
			}
			sentence := strings.LastIndexAny(answer[:at], ".!?\n") + 1
			if evalHypotheticalRe.MatchString(strings.TrimSpace(answer[sentence:at])) {
				continue
			}
			clause := answer[sentence:at]
			if i := strings.LastIndexAny(clause, ",;:—–("); i >= 0 {
				_, size := utf8.DecodeRuneInString(clause[i:])
				clause = clause[i+size:]
			}
			words := strings.Fields(clause)
			denied := false
			for i := max(len(words)-4, 0); i < len(words); i++ {
				if evalNegatorRe.MatchString(strings.Trim(words[i], "`*_\"'")) {
					denied = true
				}
			}
			if len(words) > 0 && evalModalRe.MatchString(strings.Trim(words[len(words)-1], "`*_\"'")) {
				denied = true
			}
			if !denied {
				out = append(out, strings.TrimSpace(answer[sentence:end]))
			}
		}
	}
	return out
}

// evalLink is one link of an answer: what it was written as, its scheme and target, and whether it has the one
// form the prompt asks for.
type evalLink struct {
	raw, scheme, target string
	wellFormed          bool
}

// evalLinks finds every link of answer in every Markdown form: inline links (an angle-bracketed destination and a
// title are parsed, a space after the scheme is kept as written), autolinks and reference definitions. Only a bare
// [text](block:<id>) or [text](finding:<key>) is well formed.
func evalLinks(answer string) []evalLink {
	var out []evalLink
	split := func(d string) (string, string) {
		if i := strings.IndexByte(d, ':'); i > 0 && evalSchemeRe.MatchString(d[:i]) {
			return strings.ToLower(d[:i]), d[i+1:]
		}
		return "", d
	}
	rest := evalInlineLinkRe.ReplaceAllStringFunc(answer, func(m string) string {
		raw := m[2 : len(m)-1]
		d := strings.TrimSpace(raw)
		if strings.HasPrefix(d, "<") {
			if i := strings.IndexByte(d, '>'); i > 0 {
				d = d[1:i]
			}
		} else if f := strings.Fields(d); len(f) > 0 {
			d = f[0]
		}
		scheme, target := split(d)
		out = append(out, evalLink{raw: raw, scheme: scheme, target: target, wellFormed: evalWellFormedLinkRe.MatchString(raw)})
		return strings.Repeat(" ", len(m))
	})
	for _, m := range evalAutolinkRe.FindAllStringSubmatch(rest, -1) {
		scheme, target := split(m[1])
		out = append(out, evalLink{raw: m[0], scheme: scheme, target: target})
	}
	for _, m := range evalRefDefRe.FindAllStringSubmatch(rest, -1) {
		scheme, target := split(strings.Trim(m[1], "<>"))
		out = append(out, evalLink{raw: m[0], scheme: scheme, target: target})
	}
	return out
}

// evalShownExactly reports whether id appears in shown as a whole id — not a prefix of a longer one (B1 in B12) and
// not one half of a finding key (the rule id or the hash of "missing-timeout:82f4…").
func evalShownExactly(shown, id string) bool {
	return regexp.MustCompile(`(?:^|[^A-Za-z0-9_:-])` + regexp.QuoteMeta(id) + `(?:$|[^A-Za-z0-9_:-])`).MatchString(shown)
}

// evalTokenIndex is where s first contains tok as a whole token (not inside a longer id: B1 is not in B12), -1 when it
// does not; for mentions, where "goto-antipattern" inside a finding link counts.
func evalTokenIndex(s, tok string) int {
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])(` + regexp.QuoteMeta(tok) + `)(?:$|[^A-Za-z0-9_-])`)
	if loc := re.FindStringSubmatchIndex(s); loc != nil {
		return loc[2]
	}
	return -1
}

// gradeEval grades a run: the case's expectations, then the system prompt's promises, which every case is held to —
// the turn finishes, every tool call succeeds, block:/finding: links name only ids the model was shown, no other link
// scheme is emitted, and a change is claimed only after an apply reported APPLIED.
func gradeEval(c evalCase, run evalRun) []evalCheck {
	var checks []evalCheck
	add := func(name string, pass bool, want, got string) {
		checks = append(checks, evalCheck{Name: name, Pass: pass, Want: want, Got: got})
	}
	add("turn finishes", run.done, "done", firstNonEmptyString(run.errMsg, "not done"))

	called := map[string]bool{}
	var trail, failed []string
	for _, tool := range run.trail {
		called[tool.Name] = true
		trail = append(trail, tool.Name)
		if !tool.OK {
			failed = append(failed, tool.Name+": "+tool.Summary)
		}
	}
	for _, name := range c.Expect.ToolsSubset {
		add("calls "+name, called[name], name+" in the trail", strings.Join(trail, ", "))
	}
	for _, name := range c.Expect.MustNotCall {
		add("never calls "+name, !called[name], name+" not in the trail", strings.Join(trail, ", "))
	}
	for _, name := range c.Expect.MustNotOffer {
		if run.offered != nil {
			add("never offers "+name, !run.offered[name], name+" not offered", "offered")
		}
	}
	add("every tool call succeeds", len(failed) == 0, "no error results", strings.Join(failed, "; "))

	last := -1
	for _, m := range c.Expect.MustMention {
		at := evalTokenIndex(run.answer, m)
		add("mentions "+m, at >= 0, m, excerpt(run.answer))
		if c.Expect.MentionOrder && at >= 0 {
			add("mentions "+m+" in order", at > last, "after the previous mention", fmt.Sprintf("at %d, previous at %d", at, last))
			last = at
		}
	}

	links := evalLinks(run.answer)
	for _, scheme := range c.Expect.MustLink {
		found := false
		for _, l := range links {
			if l.wellFormed && l.scheme+":" == scheme {
				found = true
			}
		}
		add("links "+scheme, found, "a "+scheme+" link", fmt.Sprintf("%d links", len(links)))
	}
	var ungrounded, foreign []string
	for _, l := range links {
		if !l.wellFormed {
			foreign = append(foreign, l.raw)
		}
		switch l.scheme {
		case "block":
			if l.target == "" || (run.blocks != nil && !run.blocks[l.target]) || !evalShownExactly(run.shown, l.target) {
				ungrounded = append(ungrounded, "block:"+l.target)
			}
		case "finding":
			if !evalFindingKeyRe.MatchString(l.target) || !evalShownExactly(run.shown, l.target) {
				ungrounded = append(ungrounded, "finding:"+l.target)
			}
		}
	}
	add("links only ids in the context", len(ungrounded) == 0, "every block:/finding: id shown to the model, exactly as shown", "not shown: "+strings.Join(ungrounded, ", "))
	add("emits only block: and finding: links", len(foreign) == 0, "only [text](block:<id>) and [text](finding:<key>)", strings.Join(foreign, ", "))

	claims := evalClaims(run.answer)
	add("claims a change only after an apply", len(claims) == 0 || run.applied(), "no claim without APPLIED", strings.Join(claims, " | "))
	if c.Expect.MustNotClaimChange {
		add("claims no change", len(claims) == 0, "no claim", strings.Join(claims, " | "))
	}
	return checks
}

func firstNonEmptyString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// excerpt is the start of an answer, for a failure message.
func excerpt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// evalFailures renders the failed checks as a diff: want, then got.
func evalFailures(checks []evalCheck) []string {
	var out []string
	for _, ch := range checks {
		if !ch.Pass {
			out = append(out, fmt.Sprintf("%s\n    want: %s\n    got:  %s", ch.Name, ch.Want, ch.Got))
		}
	}
	return out
}

// TestChatEval_Offline replays every case's transcript through the real stream and grades it.
func TestChatEval_Offline(t *testing.T) {
	cases := loadEvalCases(t)
	if len(cases) < evalMinCases {
		t.Fatalf("the eval set has %d cases, want at least %d", len(cases), evalMinCases)
	}
	flows := map[string]bool{}
	for _, c := range cases {
		flows[c.Flow] = true
		t.Run(c.name, func(t *testing.T) {
			tr := loadEvalTranscript(t, c.name)
			p := &testutil.FakeProvider{Tools: true, RequireScrubbed: t, Turns: tr.fakeTurns()}
			run := runEvalCase(t, c, keyedFactory(), "chat-eval-"+c.name, p.DefaultModel(), p, p.Requests, 20*time.Second)
			for _, f := range evalFailures(gradeEval(c, run)) {
				t.Errorf("%s", f)
			}
			// The loop asked for exactly the scripted turns: one more is a call the script did not expect (a step
			// lost), one fewer a turn the loop ended early.
			if p.Calls() != len(tr.Turns) {
				t.Errorf("the loop made %d provider calls, the transcript scripts %d", p.Calls(), len(tr.Turns))
			}
		})
	}
	if len(flows) < 8 {
		t.Errorf("the eval set covers %d corpus flows, want at least 8", len(flows))
	}
}

// TestChatEval_GraderCatchesBrokenPromises: the grader itself fails the answers the prompt forbids — a link to an id
// the model was never shown, a link of another scheme, a change claimed without an apply — and lets denials and
// conditions through.
func TestChatEval_GraderCatchesBrokenPromises(t *testing.T) {
	c := evalCase{Expect: evalExpect{MustMention: []string{"B7"}, MustLink: []string{"block:"}, MustNotClaimChange: true}}
	base := evalRun{done: true, shown: "**Block ID:** B7\n(finding id: unhandled-error:abc)"}
	failing := func(answer string) []string {
		run := base
		run.answer = answer
		var names []string
		for _, ch := range gradeEval(c, run) {
			if !ch.Pass {
				names = append(names, ch.Name)
			}
		}
		return names
	}
	if got := failing("The [IF](block:B7) branch is fine; see [the issue](finding:unhandled-error:abc). Nothing has been changed, and if you approve it will be applied."); len(got) != 0 {
		t.Errorf("a grounded, change-free answer failed %v", got)
	}
	for answer, want := range map[string]string{
		"See [the IF](block:B7) and [the loop](block:B12).":           "links only ids in the context",
		"See [the IF](block:B7), docs at [PAD](https://example.com).": "emits only block: and finding: links",
		"I've wrapped [the IF](block:B7) in an error handler.":        "claims a change only after an apply",
		"The fix has been applied to [the IF](block:B7).":             "claims no change",
		"The IF block is fine.":                                       "links block:",
		"See [the IF](block:B70).":                                    "mentions B7",
	} {
		if got := failing(answer); !containsString(got, want) {
			t.Errorf("%q: failed checks %v, want %q among them", answer, got, want)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestChatEval_GraderCatchesEvasions: the claim and link checks are not fooled by phrasing — a negation or condition
// word elsewhere in the sentence, a past participle that opens the sentence, a "now does X" state change, a rule id
// or bare hash dressed as a finding id, a spaced, titled or angle-bracketed link target, an autolink, a reference
// definition — while the denials and conditions a correct answer uses still pass.
func TestChatEval_GraderCatchesEvasions(t *testing.T) {
	shown := "**Block ID:** B7\n(finding id: unhandled-error:abc123)\n" +
		"[warning/Reliability] missing-timeout: Network operation without timeout — block=B4 key=missing-timeout:82f47428193b9166\n"
	base := evalRun{done: true, shown: shown, blocks: map[string]bool{"B4": true, "B7": true}}
	failing := func(c evalCase, answer string) []string {
		run := base
		run.answer = answer
		var names []string
		for _, ch := range gradeEval(c, run) {
			if !ch.Pass {
				names = append(names, ch.Name)
			}
		}
		return names
	}
	noClaim := evalCase{Expect: evalExpect{MustNotClaimChange: true}}
	for _, answer := range []string{
		"No problem, I've applied the fix to B4.",
		"As you asked, I can confirm I've fixed B4.",
		"When you asked, I updated the IF on line 8.",
		"I went ahead and applied the timeout fix.",
		"Applied the fix to B4.",
		"B4 now reads the password from the credential store.",
		"Done. Fixed it: B4 has a timeout now.",
		"Sure — the timeout fix has been applied to B4.",
	} {
		got := failing(noClaim, answer)
		if !containsString(got, "claims a change only after an apply") || !containsString(got, "claims no change") {
			t.Errorf("%q: a change claim got through (failed checks %v)", answer, got)
		}
	}
	for answer, want := range map[string]string{
		"See [the timeout](finding:missing-timeout).":          "links only ids in the context",
		"See [the timeout](finding:82f47428193b9166).":         "links only ids in the context",
		"See [the block](block: B99).":                         "emits only block: and finding: links",
		`See [the block](block:B99 "title").`:                  "links only ids in the context",
		"See [the block](<block:B99>).":                        "links only ids in the context",
		"See <https://example.com/pad>.":                       "emits only block: and finding: links",
		"See <block:B7>.":                                      "emits only block: and finding: links",
		"See [the docs][1].\n\n[1]: https://example.com":       "emits only block: and finding: links",
		"See [the docs](https://example.com/a_(b)).":           "emits only block: and finding: links",
		"See [the issue](block:unhandled-error:abc123).":       "links only ids in the context",
		"See [the issue](finding:unhandled-error:abc123 'x').": "emits only block: and finding: links",
	} {
		if got := failing(evalCase{}, answer); !containsString(got, want) {
			t.Errorf("%q: failed checks %v, want %q among them", answer, got, want)
		}
	}
	for _, answer := range []string{
		"[IF %ButtonPressed% = OK](block:B7) is fine; see [the issue](finding:unhandled-error:abc123) and [timeouts](finding:missing-timeout:82f47428193b9166). Nothing has been changed, and if you approve it will be applied.",
		"You declined the change, so nothing was changed in the flow.",
		"This is a preview only — nothing has been changed. Ask me to apply it and you will get an approval prompt first.",
		"I can't apply this one from chat. Nothing was changed.",
		"Once applied, [the block](block:B4) waits at most 30 seconds.",
		"If you approve, the fix will be applied to [the block](block:B4).",
		"`Item` is read in [the IF](block:B7); no other action writes it.",
		"The variable was changed on line 12 and read on line 13; plain URLs like https://example.com stay text.",
		"It never runs, so nobody has changed it since the import.",
	} {
		if got := failing(noClaim, answer); len(got) != 0 {
			t.Errorf("%q: a correct answer failed %v", answer, got)
		}
	}
}

// TestEvalLiveFactory_OneAttemptPerCall: the live runner's chain makes one attempt per call, so a retried attempt is
// never recorded as an extra transcript turn (-record-evals) and a case's calls are exactly the loop's.
func TestEvalLiveFactory_OneAttemptPerCall(t *testing.T) {
	p := &testutil.FakeProvider{Err: ai.ErrProviderDown}
	ai.RegisterProvider("chat-eval-retry-probe", func(string) ai.Provider { return p })
	chain, err := evalLiveFactory().For(context.Background(), "u1", "chat-eval-retry-probe")
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.Stream(context.Background(), ai.Request{Model: "m"}, func(ai.Chunk) {}); !errors.Is(err, ai.ErrProviderDown) {
		t.Fatalf("Stream: %v, want the provider's error", err)
	}
	if n := len(p.Requests()); n != 1 {
		t.Errorf("the provider saw %d attempts of one call, want 1", n)
	}
}
