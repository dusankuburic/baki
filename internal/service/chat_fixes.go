package service

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/ai/scrubber"
	"pad-core/logger"
	"pad-core/models"

	"github.com/google/uuid"
)

// errFixNeedsWriteScope refuses a chat fix for a scoped API token without the write scope. The HTTP scope gate
// never sees this mutation: it runs inside the chat stream, which a chat-only token is allowed to start.
var errFixNeedsWriteScope = fmt.Errorf("%w: this API token lacks the %q scope needed to apply fixes", ErrPermissionDenied, auth.ScopeWrite)

// fixScopeRefusal is the model-readable answer when the stream's token may not apply fixes.
const fixScopeRefusal = "this session's API token is not allowed to modify flows (it lacks the write scope) — nothing was changed. Explain the fix instead so the user can apply it."

// requireFixWriteScope allows interactive sessions and unscoped tokens; a scoped token needs ScopeWrite. It is
// FlowEdit's actor stage (CanAttempt): no flow is at hand when the approval is offered, and the editor check that
// follows is GetAuthorizedFlow's. Without an actor (local mode) there is nothing to restrict.
func requireFixWriteScope(ctx context.Context) error {
	actor := auth.ActorFromContext(ctx)
	if actor == nil {
		return nil
	}
	err := CanAttempt(actor, FlowEdit)
	if errors.Is(err, ErrScopeDenied) {
		return errFixNeedsWriteScope
	}
	return err
}

// chatFixApplier implements ai.ToolFixApplier for one chat stream: emits the fix_proposal SSE event, waits for
// the decision, and on approval applies the fix to the REAL flow through FlowService.ApplyFix (the patch is
// re-computed server-side on the unscrubbed source — the scrubbed doc only ever produced the preview),
// re-analyzes, and reports whether the finding resolved.
type chatFixApplier struct {
	svc      *ChatService
	doc      *models.FlowDocument // pre-fix snapshot: used for flow identity/authorization
	streamID string
	emit     func(eventType string, data map[string]interface{})
	touch    func()
	// onDocReplaced, when set, is invoked with the fresh post-fix document so the owning tool loop can swap its
	// working copy mid-stream (A1) — the re-parse mints new block IDs, and follow-up tools on the stale snapshot
	// would mis-answer.
	onDocReplaced func(fresh *models.FlowDocument) error

	// grounded is the conversation's context block, by its id in groundedDoc (the latest version of the flow
	// the stream has seen): each apply re-parses the flow, and its result tells the model the block's new id.
	groundMu    sync.Mutex
	grounded    string
	groundedDoc *models.FlowDocument
}

// groundOn records the conversation's context block, an id in doc, to follow across the stream's applies.
func (a *chatFixApplier) groundOn(doc *models.FlowDocument, blockID string) {
	a.groundMu.Lock()
	a.grounded, a.groundedDoc = blockID, doc
	a.groundMu.Unlock()
}

// followGrounded moves the grounded block to its id in fresh, the re-parsed flow, by its content anchor, and
// returns the sentence telling the model where it went; "" with no grounded block or no re-parse.
func (a *chatFixApplier) followGrounded(fresh *models.FlowDocument) string {
	a.groundMu.Lock()
	defer a.groundMu.Unlock()
	if a.grounded == "" || fresh == nil {
		return ""
	}
	anchor := a.groundedDoc.AnchorOf(a.grounded)
	a.grounded, a.groundedDoc = "", fresh
	if anchor != nil {
		a.grounded = fresh.ResolveAnchor(*anchor)
	}
	if a.grounded == "" {
		return " The block in the context's Selected Block section is no longer in the flow."
	}
	return " The block in the context's Selected Block section is now block id " + a.grounded + "."
}

// newFixApplier builds the per-stream applier; nil when the service can't apply fixes (no flow service wired),
// leaving tctx.Fixes nil and apply_fix to report "not available".
func (s *ChatService) newFixApplier(doc *models.FlowDocument, streamID string, emit func(string, map[string]interface{}), touch func(), onDocReplaced func(*models.FlowDocument) error) ai.ToolFixApplier {
	if s.flowCache == nil || emit == nil {
		return nil
	}
	return &chatFixApplier{svc: s, doc: doc, streamID: streamID, emit: emit, touch: touch, onDocReplaced: onDocReplaced}
}

// The apply + verify window gets its own deadline, started when the user approves (R-approvals-3): a slow decision
// no longer eats into the write, and a batch is sized per item. Vars so tests can shrink them.
var (
	fixApplyBase    = 30 * time.Second
	fixApplyPerItem = 30 * time.Second
)

// fixApplyTimeout bounds applying and verifying n approved fixes.
func fixApplyTimeout(n int) time.Duration {
	return fixApplyBase + time.Duration(n)*fixApplyPerItem
}

// applyTimedOut reports whether the apply window's own deadline ended it while the stream is still alive.
func applyTimedOut(stream, apply context.Context) bool {
	return stream.Err() == nil && errors.Is(apply.Err(), context.DeadlineExceeded)
}

// fixApplyTimeoutResult is the model-readable result when the apply window ran out before the outcome was known.
func fixApplyTimeoutResult(tool string, budget time.Duration) string {
	return fmt.Sprintf("error: %s did not finish within %s — the flow may already have changed; ask the user to check it before trying again", tool, budget)
}

// fixRun tracks one approval card through an applier, so a panic can still resolve it (recoverFixPanic).
type fixRun struct {
	id   string
	open bool // the card is shown and no terminal decision has been emitted
}

// decide emits a terminal decision for the run's card and closes it.
func (a *chatFixApplier) decide(run *fixRun, status, message string) {
	run.open = false
	a.emitFixDecision(run.id, status, message)
}

// recoverFixPanic is deferred by both appliers: a panic on their path (model-controlled input reaching a bug in a
// fixer, a patch builder or the flow write) becomes the error-shaped result tool failures surface to the model, and a
// card still open is resolved as error — not left waiting while the stream ends as interrupted (R-tool-loop-7). The
// write may already have landed, so the result says to check the flow.
func (a *chatFixApplier) recoverFixPanic(ctx context.Context, tool string, run *fixRun, out *string, err *error) {
	r := recover()
	if r == nil {
		return
	}
	logger.FromContext(ctx).Error("chat: fix applier panicked", "tool", tool, "panic", r, "stack", string(debug.Stack()))
	if run.open {
		a.decide(run, "error", "the fix failed unexpectedly")
	}
	*out = scrubber.ScrubText(fmt.Sprintf("error: tool %s failed unexpectedly (%v) — the flow may already have changed; ask the user to check it before trying again", tool, r))
	*err = nil
}

// ApplyFixWithApproval implements ai.ToolFixApplier: the returned string is always model-readable on nil-error
// paths; an error means the stream context died and the loop's failure handling takes over.
func (a *chatFixApplier) ApplyFixWithApproval(ctx context.Context, prop ai.FixProposal) (out string, err error) {
	run := &fixRun{id: prop.ProposalID}
	defer a.recoverFixPanic(ctx, "apply_fix", run, &out, &err)
	if a.touch != nil {
		a.touch()
	}
	// Refuse before prompting: an approval the token could never act on would only waste the user's click.
	if requireFixWriteScope(ctx) != nil {
		return fixScopeRefusal, nil
	}
	// One item, repeated as flat fields: the card reads items[]; clients before it read the flat fields (the
	// item's id stays inside items[]: the event's own id is proposalId).
	item := fixProposalItem(prop)
	flat := make(map[string]interface{}, len(item))
	for k, v := range item {
		if k != "id" {
			flat[k] = v
		}
	}
	run.open = true
	dec, ended := a.svc.requestApproval(ctx, a.streamID, approvalRequestFor(fixToolSpec("apply_fix"), prop.ProposalID, []map[string]interface{}{item}, flat), a.emit, a.touch)
	fixMeta := map[string]string{"ruleId": prop.RuleID, "fixType": prop.FixType, "blockId": prop.BlockID}
	a.auditDecision(ctx, prop.ProposalID, dec, ended, fixMeta)
	switch ended {
	case approvalCancelled:
		run.open = false
		return "", ctx.Err()
	case approvalTimedOut:
		run.open = false
		return "the user did not respond to the approval prompt in time — nothing was changed. Ask them whether to try again or apply it manually.", nil
	case approvalRefused:
		run.open = false
		return drainingApprovalRefusal, nil
	}
	if !dec.approved {
		a.decide(run, "declined", "")
		return "the user DECLINED this fix — nothing was changed. Do not call apply_fix again for the same finding unless the user explicitly asks; explain the change instead.", nil
	}
	// From here the approved fix is a critical section: a stop (the user's or the server's) waits for it.
	release, held := a.svc.holdStreamCancel(a.streamID)
	if !held {
		a.decide(run, "cancelled", approvalEndedBeforeApply)
		return "", context.Canceled
	}
	defer release()

	a.emitFixDecision(prop.ProposalID, "applying", "")
	// Keepalive through the apply+verify window (L4): no provider chunks flow then — a >90s re-analysis would
	// otherwise trip the idle watchdog although the apply window allows longer. Closed via defer so a panic in
	// the apply path cannot strand the goroutine.
	applyDone := a.beginApplyKeepalive(ctx)
	defer close(applyDone)
	budget := fixApplyTimeout(1)
	applyCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	outcome, updated, err := a.svc.applyApprovedFix(applyCtx, a.doc, prop)
	a.auditApply(ctx, prop.ProposalID, outcome.Status, err, fixMeta)
	if err != nil {
		if applyTimedOut(ctx, applyCtx) {
			a.decide(run, "error", "the fix did not finish in time")
			return fixApplyTimeoutResult("apply_fix", budget), nil
		}
		// Only an ended stream returns an error: the card says why, never the raw context error.
		a.decide(run, "error", a.svc.endedDuringApply(a.streamID))
		return "", err
	}
	// A1: hand the fresh post-fix document to the owning loop BEFORE the decision event, so the model's next
	// tool call runs against the fixed flow (new block IDs included).
	note := a.handOver(updated)
	a.decide(run, outcome.Status, outcome.Detail)
	return outcome.ModelSummary() + a.followGrounded(updated) + note, nil
}

// staleToolsNote tells the model that the fix is in the flow but the tools still read the previous clone
// (toolDocSwap could not make the fresh one's), so its next reads are of the flow as it was.
const staleToolsNote = "\nNote: the fix is applied to the flow, but its updated copy could not be prepared for you, so the read tools still show the flow as it was before this fix. Tell the user to start a new turn to continue."

// handOver gives the owning loop the fresh document; the note it returns is for the model when the loop could not
// take it.
func (a *chatFixApplier) handOver(updated *models.FlowDocument) string {
	if updated == nil || a.onDocReplaced == nil {
		return ""
	}
	if err := a.onDocReplaced(updated); err != nil {
		return staleToolsNote
	}
	return ""
}

// toolDocSwap is the owning loop's half of an applied fix: the fresh document replaces the real one the applier
// works on, and its redacting clone replaces what the tools read. A clone that cannot be made leaves the tools on
// the previous clone — never the document itself — and the error reaches the model through the applier's note.
func toolDocSwap(tctx *ai.ToolContext) func(*models.FlowDocument) error {
	return func(fresh *models.FlowDocument) error {
		tctx.RealDoc = fresh
		sd, err := scrubDocument(fresh)
		if err != nil {
			logger.FromContext(tctx.Ctx).Error("chat: the post-fix flow's redacting clone could not be made; tools keep the previous clone", "error", err)
			return err
		}
		tctx.Doc = sd
		return nil
	}
}

func (a *chatFixApplier) emitFixDecision(proposalID, status, message string) {
	emitApprovalDecision(a.emit, proposalID, status, message)
}

// fixToolSpec returns the registry's spec of a fix tool; the applier only serves registered tools.
func fixToolSpec(name string) ai.ToolSpec {
	spec, ok := ai.ToolSpecOf(name)
	if !ok {
		panic("chat fixes: unknown tool " + name)
	}
	return spec
}

// fixItemID identifies one fix inside a request and its decision: a batch may fix the same rule on several
// blocks, so ruleId alone cannot pair an outcome with its item. Clients prefer id when present.
func fixItemID(p ai.FixProposal) string { return p.BlockID + ":" + p.RuleID }

// fixProposalItem renders one proposal as a fix-patch item of the approval card.
func fixProposalItem(p ai.FixProposal) map[string]interface{} {
	return map[string]interface{}{
		"id":      fixItemID(p),
		"ruleId":  p.RuleID,
		"fixType": p.FixType,
		"blockId": p.BlockID,
		// Block names are never masked by ScrubDocument (only Properties) — scrub the label as defense in depth.
		"blockLabel": scrubber.ScrubText(p.BlockLabel),
		"line":       p.Line,
		"summary":    p.Summary,
	}
}

// beginApplyKeepalive starts the apply-window keepalive: a ticker goroutine touching the activity clock while
// an apply + re-analysis runs. The caller MUST close the returned channel via defer immediately, so a PANIC in
// the apply path still ends the window (a plain close-after-the-call unwound past it and stranded the ticker
// goroutine forever). The goroutine also watches ctx, so a hung apply cannot outlive the stream.
func (a *chatFixApplier) beginApplyKeepalive(ctx context.Context) chan struct{} {
	applyDone := make(chan struct{})
	if a.touch == nil {
		return applyDone
	}
	keepalive := time.NewTicker(fixDecisionKeepalive)
	go func() {
		defer keepalive.Stop()
		for {
			select {
			case <-keepalive.C:
				a.touch()
			case <-applyDone:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return applyDone
}

// ApplyFixesWithApproval implements the batch half of ai.ToolFixApplier: ONE approval prompt, one decision, then
// a sequential apply with per-item verification (see applyApprovedFixes).
func (a *chatFixApplier) ApplyFixesWithApproval(ctx context.Context, props []ai.FixProposal) (out string, err error) {
	run := &fixRun{}
	defer a.recoverFixPanic(ctx, "apply_fixes", run, &out, &err)
	if len(props) == 0 {
		return "error: empty batch", nil
	}
	if a.touch != nil {
		a.touch()
	}
	if requireFixWriteScope(ctx) != nil {
		return fixScopeRefusal, nil
	}
	batchID := props[0].ProposalID + "-batch-" + uuid.NewString()[:8]
	items := make([]map[string]interface{}, len(props))
	for i, p := range props {
		items[i] = fixProposalItem(p)
	}
	run.id, run.open = batchID, true
	dec, ended := a.svc.requestApproval(ctx, a.streamID, approvalRequestFor(fixToolSpec("apply_fixes"), batchID, items,
		map[string]interface{}{"batch": true, "count": len(props)}), a.emit, a.touch)
	batchMeta := map[string]string{"count": strconv.Itoa(len(props))}
	if dec.approved {
		batchMeta["excludedCount"] = strconv.Itoa(len(dec.excluded))
	}
	a.auditDecision(ctx, batchID, dec, ended, batchMeta)
	delete(batchMeta, "excludedCount")
	switch ended {
	case approvalCancelled:
		run.open = false
		return "", ctx.Err()
	case approvalTimedOut:
		run.open = false
		return "the user did not respond to the batch approval in time — nothing was changed. Ask whether to try again.", nil
	case approvalRefused:
		run.open = false
		return drainingApprovalRefusal, nil
	}
	// excluded is the winning decision's per-item opt-outs, received with it.
	excluded := dec.excluded
	if !dec.approved {
		a.decide(run, "declined", "")
		return "the user DECLINED the batch — nothing was changed. Do not re-propose the same batch unless asked; explain the changes instead.", nil
	}

	// Per-item opt-out (U4.1): apply only the items the user left checked (indices == props order); the others are
	// reported as skipped, in their place.
	all, skip := props, make(map[int]bool, len(excluded))
	for _, i := range excluded {
		if i >= 0 && i < len(all) {
			skip[i] = true
		}
	}
	if len(skip) > 0 {
		props = make([]ai.FixProposal, 0, len(all))
		for i, p := range all {
			if !skip[i] {
				props = append(props, p)
			}
		}
		if len(props) == 0 {
			a.decide(run, "declined", "every fix in the batch was deselected")
			return "the user deselected every fix in the batch — nothing was changed.", nil
		}
	}

	release, held := a.svc.holdStreamCancel(a.streamID)
	if !held {
		a.decide(run, "cancelled", approvalEndedBeforeApply)
		return "", context.Canceled
	}
	defer release()
	a.emitFixDecision(batchID, "applying", "")
	// Keepalive through the whole multi-apply window (see ApplyFixWithApproval); closed via defer.
	applyDone := a.beginApplyKeepalive(ctx)
	defer close(applyDone)
	budget := fixApplyTimeout(len(props))
	applyCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	outcome, updated, err := a.svc.applyApprovedFixes(applyCtx, a.doc, props, a.svc.stopBetweenFixes(a.streamID))
	batchMeta["applied"] = strconv.Itoa(outcome.appliedCount())
	a.auditApply(ctx, batchID, outcome.Status, err, batchMeta)
	if err != nil {
		if applyTimedOut(ctx, applyCtx) {
			a.decide(run, "error", "the batch did not finish in time")
			return fixApplyTimeoutResult("apply_fixes", budget), nil
		}
		a.decide(run, "error", a.svc.endedDuringApply(a.streamID))
		return "", err
	}
	outcome.Items = withSkipped(all, skip, outcome.Items, outcome.Detail)
	note := a.handOver(updated)
	run.open = false
	a.emitBatchDecision(batchID, outcome)
	return outcome.ModelSummary() + a.followGrounded(updated) + note, nil
}

// auditDecision records the user's decision on a card (chat.fix_decision), under the stream's owner: the stream's
// context carries the claims of the request that started it. A card never shown (draining) records nothing.
func (a *chatFixApplier) auditDecision(ctx context.Context, proposalID string, dec approvalDecision, ended approvalOutcome, meta map[string]string) {
	status := ""
	switch ended {
	case approvalDecided:
		status = "declined"
		if dec.approved {
			status = "approved"
		}
	case approvalTimedOut:
		status = "timeout"
	case approvalCancelled:
		status = "cancelled"
	default:
		return
	}
	a.recordAudit(ctx, audit.ActionChatFixDecision, proposalID, meta, map[string]string{"status": status})
}

// auditApply records an approved fix's write (flow.fix_apply) with how it ended; an ended stream is an "error". An
// apply that wrote nothing (the finding was already gone) is no write: the decision's row is its record.
func (a *chatFixApplier) auditApply(ctx context.Context, proposalID, status string, err error, meta map[string]string) {
	if err == nil && (status == "already-resolved" || meta["applied"] == "0" && status != "error") {
		return
	}
	if err != nil || status == "" {
		status = "error"
	}
	a.recordAudit(ctx, audit.ActionFlowFixApply, proposalID, meta, map[string]string{"source": "chat", "unsafe": "false", "outcome": status})
}

func (a *chatFixApplier) recordAudit(ctx context.Context, action, proposalID string, metas ...map[string]string) {
	if a.svc.audit == nil || a.doc == nil {
		return
	}
	m := map[string]string{"streamId": a.streamID, "proposalId": proposalID}
	for _, meta := range metas {
		for k, v := range meta {
			m[k] = v
		}
	}
	a.svc.audit.Record(ctx, audit.Entry{Action: action, ResourceType: "flow", ResourceID: a.doc.ID, OrgID: a.doc.OrganizationID, Meta: m})
}

// skippedItemMessage is the outcome of a batch item the user deselected.
const skippedItemMessage = "deselected by the user — nothing was changed for this fix"

// withSkipped puts the deselected items of all back in their place among the outcomes of the applied ones. A batch
// that failed before its first fix has no outcome per item: each kept item then reports the batch's reason.
func withSkipped(all []ai.FixProposal, skip map[int]bool, applied []batchItemOutcome, detail string) []batchItemOutcome {
	out := make([]batchItemOutcome, 0, len(all))
	next := 0
	for i, p := range all {
		if skip[i] {
			out = append(out, batchItemOutcome{ID: fixItemID(p), RuleID: p.RuleID, Status: "skipped", Message: skippedItemMessage})
			continue
		}
		if next < len(applied) {
			out = append(out, applied[next])
			next++
			continue
		}
		msg := detail
		if msg == "" {
			msg = "not attempted"
		}
		out = append(out, batchItemOutcome{ID: fixItemID(p), RuleID: p.RuleID, Status: "error", Message: msg})
	}
	return out
}

// approvalEndedBeforeApply is the card's message when the stream was ending as the approval arrived.
const approvalEndedBeforeApply = "the response ended before the fix was applied — nothing was changed"

// holdStreamCancel holds the stream's cancel through an approved apply (streamCtl.holdCancel); a stream this service
// does not run (tests driving an applier directly) has nothing to hold.
func (s *ChatService) holdStreamCancel(streamID string) (release func(), ok bool) {
	v, found := s.activeStreams.Load(streamID)
	if !found {
		return func() {}, true
	}
	return v.(*streamCtl).holdCancel()
}

// stopBetweenFixes returns the batch's check before each fix: once a stop was asked for (held by the apply), the
// fixes not yet written are not attempted, and its answer says why; "" to go on.
func (s *ChatService) stopBetweenFixes(streamID string) func() string {
	return func() string {
		v, found := s.activeStreams.Load(streamID)
		if !found {
			return ""
		}
		code, asked := v.(*streamCtl).stopRequested()
		switch {
		case !asked:
			return ""
		case code == ChatErrServerRestarting:
			return "not attempted: the server is restarting — nothing was changed for this fix"
		case code == ChatErrIdleTimeout:
			return "not attempted: the response was stopped because it stopped making progress — nothing was changed for this fix"
		case code == ChatErrClientGone:
			return "not attempted: the response was stopped because you were disconnected — nothing was changed for this fix"
		default:
			return "not attempted: the response was stopped — nothing was changed for this fix"
		}
	}
}

// endedDuringApply is the card's message when the stream ended while an approved fix was being applied.
func (s *ChatService) endedDuringApply(streamID string) string {
	if v, ok := s.activeStreams.Load(streamID); ok {
		ctl := v.(*streamCtl)
		ctl.mu.Lock()
		code := ctl.cancelCode
		ctl.mu.Unlock()
		if code == ChatErrServerRestarting {
			return "the server restarted before the fix finished — check the flow before trying again"
		}
	}
	return "the response ended before the fix finished — check the flow before trying again"
}

func (a *chatFixApplier) emitBatchDecision(batchID string, outcome batchOutcome) {
	recordChatFixDecision(outcome.Status)
	data := map[string]interface{}{
		"proposalId": batchID,
		"status":     outcome.Status,
	}
	if outcome.Detail != "" {
		data["message"] = outcome.Detail
	}
	items := make([]map[string]interface{}, len(outcome.Items))
	for i, it := range outcome.Items {
		m := map[string]interface{}{"id": it.ID, "ruleId": it.RuleID, "status": it.Status}
		if it.Message != "" {
			m["message"] = it.Message
		}
		items[i] = m
	}
	data["items"] = items
	a.emit("fix_decision", data)
}

// chatFixOutcome is the verified result of an approved apply.
type chatFixOutcome struct {
	Status string // "applied" | "applied-unresolved" | "already-resolved" | "error"
	Detail string
	// Unverified: applied, but the verifying re-analysis ran out of the apply window, so the outcome is
	// unknown rather than unresolved (R-approvals-3).
	Unverified bool
}

// ModelSummary renders the outcome for the model's next turn. Scrubbed: the
// detail can carry apply-path error text, and model-visible strings never
// bypass the scrubber (chat messages, tool results, and summaries all pass
// through it; the provider boundary is not re-scrubbing here).
func (o chatFixOutcome) ModelSummary() string {
	switch o.Status {
	case "applied":
		if o.Unverified {
			return scrubber.ScrubText("APPLIED, not verified: " + o.Detail)
		}
		return scrubber.ScrubText("APPLIED and verified: " + o.Detail)
	case "applied-unresolved":
		return scrubber.ScrubText("APPLIED, but " + o.Detail)
	case "already-resolved":
		return scrubber.ScrubText("NOT APPLIED, nothing to do: " + o.Detail)
	default:
		return scrubber.ScrubText("apply failed: " + o.Detail)
	}
}

// applyFixFunc is the FlowService-backed mutation hook — a field so tests can substitute it.
type applyFixFunc func(ctx context.Context, doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (*models.FlowDocument, error)

func (s *ChatService) applyFixHook() applyFixFunc {
	if s.applyFixFunc != nil {
		return s.applyFixFunc
	}
	return func(ctx context.Context, doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (*models.FlowDocument, error) {
		// unsafe=false: a chat approval is a one-line card with no diff — exactly the unattended approval the
		// behaviour-changing fixers are gated against. Those report the opt-in error here and stay available from
		// the finding card, where the preview diff is shown first.
		return s.flowCache.ApplyFix(ctx, doc, blockID, fixType, ruleID, variable, property, false)
	}
}

// fixConflictAttempts bounds the writes of one approved fix: a version conflict (a save between the version read and
// the write) is retried once on the flow as it is then.
const fixConflictAttempts = 2

// alreadyResolvedDetail is the outcome of an approved fix whose finding was gone before it could be applied.
const alreadyResolvedDetail = "the finding no longer appears on the flow (an edit since the proposal resolved or removed it) — nothing was changed"

// applyApprovedFix applies an approved proposal against the REAL flow and verifies it: re-resolves a fresh
// document the stream's actor may edit (FlowEdit; the stream itself only required ChatUse), finds the proposal's
// finding on it again (an edit during the approval wait re-parses the flow: new block ids), runs the fix (patch
// recomputed on the unscrubbed source), invalidates the cached chat context, and re-analyzes to check whether the
// finding (by its re-parse-stable fingerprint) disappeared. The updated document is returned for the caller's
// refresh (A1). A fix that could not be applied is an "error" outcome in words the model can act on; an error is
// returned only when ctx ended.
//
// ctx carries the apply window's deadline (fixApplyTimeout). The window is keepalive-touched: without touches the
// idle watchdog would cancel a healthy stream during a >90s re-analysis.
func (s *ChatService) applyApprovedFix(ctx context.Context, snapshot *models.FlowDocument, prop ai.FixProposal) (chatFixOutcome, *models.FlowDocument, error) {
	if snapshot == nil {
		return chatFixOutcome{Status: "error", Detail: "no flow is loaded — nothing was changed"}, nil, nil
	}
	// Re-checked at apply time (defense in depth): this is the mutation itself.
	if err := requireFixWriteScope(ctx); err != nil {
		return s.fixFailure(ctx, err)
	}
	var doc, updated *models.FlowDocument
	for attempt := 1; ; attempt++ {
		// Fresh doc + FlowEdit check at apply time: the stream was authorized for ChatUse and the snapshot may be stale.
		var err error
		doc, err = s.GetAuthorizedFlow(ctx, snapshot.ID, FlowEdit)
		if err != nil {
			return s.fixFailure(ctx, err)
		}
		blockID, gone, err := s.locateApproved(ctx, doc, fixTargetOf(prop, snapshot, doc))
		if err != nil {
			return s.fixFailure(ctx, err)
		}
		if gone {
			return chatFixOutcome{Status: "already-resolved", Detail: alreadyResolvedDetail}, nil, nil
		}
		RecordFlowOp("chat_apply_fix")
		updated, err = s.applyFixHook()(ctx, doc, blockID, prop.FixType, prop.RuleID, prop.Variable, prop.Property)
		if errors.Is(err, storageif.ErrVersionConflict) && attempt < fixConflictAttempts {
			continue
		}
		if err != nil {
			return s.fixFailure(ctx, err)
		}
		break
	}

	// The flow changed under the chat's cached (scrubbed) context — drop it for the fixed source.
	s.InvalidateChatContext(doc.ID)

	out := s.verifyAppliedFix(ctx, prop, updated)
	if updated != nil {
		// The apply re-parsed the source, minting fresh block IDs: every ID the model saw is stale, whatever the
		// verification found, so say so explicitly — otherwise its next apply_fix targets a dead ID.
		out.Detail += reparsedNote
	}
	return out, updated, nil
}

// fixFailure is an approved fix that was not applied, in words the card and the model can act on: the raw error
// (storage text, a wrapped context error) is logged, never shown. An ended ctx is returned as the error — the
// stream is going, and the applier words that.
func (s *ChatService) fixFailure(ctx context.Context, err error) (chatFixOutcome, *models.FlowDocument, error) {
	if ctx.Err() != nil {
		return chatFixOutcome{Status: "error"}, nil, ctx.Err()
	}
	logger.FromContext(ctx).Warn("chat: an approved fix was not applied", "error", err)
	return chatFixOutcome{Status: "error", Detail: fixFailureDetail(err)}, nil, nil
}

// fixFailureDetail words why an approved fix was not applied.
func fixFailureDetail(err error) string {
	switch {
	case errors.Is(err, storageif.ErrVersionConflict):
		return "the flow changed while the fix was being applied, so nothing was changed — call list_findings and try again"
	case errors.Is(err, errFixNeedsWriteScope):
		return "this session's API token is not allowed to modify flows (it lacks the write scope) — nothing was changed"
	case errors.Is(err, ErrPermissionDenied):
		return "the user is not allowed to edit this flow — nothing was changed"
	case errors.Is(err, ErrNotFound), errors.Is(err, storageif.ErrNotFound):
		return "the flow could not be found (it may have been deleted) — nothing was changed"
	default:
		return "the fix could not be applied to the flow as it is now, so nothing was changed — call list_findings and try again"
	}
}

// locateApproved finds the approved finding on doc as it is now: the block id to fix, or gone when the finding no
// longer appears. Without an analysis it trusts the proposal's id, as before re-association.
func (s *ChatService) locateApproved(ctx context.Context, doc *models.FlowDocument, t fixTarget) (blockID string, gone bool, err error) {
	if s.analysisCache == nil {
		return t.prop.BlockID, false, nil
	}
	report, aerr := s.analysisCache.AnalyzeFlowReadOnly(ctx, doc)
	if aerr != nil || report == nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return t.prop.BlockID, false, nil
	}
	f := t.locate(report, doc, map[int]bool{})
	if f == nil {
		return "", true, nil
	}
	return f.BlockID, false, nil
}

// reparsedNote ends the outcome of every apply that returned a re-parsed document.
const reparsedNote = ". The flow was re-parsed: ALL block IDs have changed — call list_findings again before referencing any block"

// verifyAppliedFix re-analyzes the applied document under ctx (the apply window) and reports whether the finding,
// by its re-parse-stable fingerprint, is gone.
func (s *ChatService) verifyAppliedFix(ctx context.Context, prop ai.FixProposal, updated *models.FlowDocument) chatFixOutcome {
	if updated == nil || s.analysisCache == nil {
		return chatFixOutcome{
			Status: "applied-unresolved",
			Detail: "the fix was applied, but the result could not be re-analyzed to verify it",
		}
	}
	report, err := s.analysisCache.AnalyzeFlow(ctx, updated)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// The write landed; only the check ran out of time — that is not evidence the fix failed.
		return chatFixOutcome{
			Status:     "applied",
			Detail:     "the fix was applied, but verification was skipped because re-analysis ran out of time — ask the user to re-run the analysis to confirm",
			Unverified: true,
		}
	}
	if err != nil || report == nil {
		return chatFixOutcome{
			Status: "applied-unresolved",
			Detail: "the fix was applied, but re-analysis failed so it could not be verified",
		}
	}

	// Verify by fingerprint: the re-parse mints fresh block IDs, so BlockID comparisons would always "resolve".
	// An empty fingerprint (older report) falls back to rule+block matching.
	stillPresent := false
	for i := range report.Findings {
		f := &report.Findings[i]
		if prop.Fingerprint != "" {
			if f.Fingerprint == prop.Fingerprint {
				stillPresent = true
				break
			}
			continue
		}
		if f.RuleID == prop.RuleID && f.BlockID == prop.BlockID {
			stillPresent = true
			break
		}
	}

	if stillPresent {
		return chatFixOutcome{
			Status: "applied-unresolved",
			Detail: fmt.Sprintf("the finding still appears after re-analysis (flow now has %d findings) — the automated fix was not sufficient, manual review recommended", len(report.Findings)),
		}
	}
	logger.FromContext(ctx).Info("chat-driven fix applied", "flow", updated.ID, "rule", prop.RuleID, "fix", prop.FixType)
	detail := fmt.Sprintf("the finding no longer appears after re-analysis (flow now has %d findings)", len(report.Findings))
	return chatFixOutcome{Status: "applied", Detail: detail}
}

// fixContentKey is a finding's identity by content, the last resort of re-association (fixTarget.locate) when a
// block's anchor no longer resolves: a patch shifts line numbers and the re-parse mints fresh block IDs +
// fingerprints, so neither survives. Truly identical sibling blocks share a key — items consume matches in order,
// which may swap siblings, but identical siblings get identical fixes.
type fixContentKey struct {
	RuleID, Subflow, RawType, Name, Subject string
}

func fixContentKeyOf(ruleID string, f *models.Finding, doc *models.FlowDocument) fixContentKey {
	k := fixContentKey{RuleID: ruleID, Subject: findingSubject(f)}
	if doc == nil {
		return k
	}
	if b := doc.BlocksByID[f.BlockID]; b != nil {
		k.RawType, k.Name = b.RawType, b.Name
		if sf := doc.BlockSubflow[f.BlockID]; sf != nil {
			k.Subflow = sf.Name
		}
	}
	return k
}

// findingSubject is the property or variable a finding is about ("" for a block-level finding).
func findingSubject(f *models.Finding) string {
	if v, ok := f.Metadata["property"].(string); ok && v != "" {
		return "property:" + v
	}
	if v, ok := f.Metadata["variable"].(string); ok && v != "" {
		return "variable:" + v
	}
	return ""
}

// proposalSubject is findingSubject of the finding a proposal was made for.
func proposalSubject(p ai.FixProposal) string {
	switch {
	case p.Property != "":
		return "property:" + p.Property
	case p.Variable != "":
		return "variable:" + p.Variable
	}
	return ""
}

// fixTarget is where an approved proposal's finding is looked for on a flow that may have been re-parsed since the
// proposal (an edit during the approval wait, an earlier fix of the same batch) — R-approvals-2.
type fixTarget struct {
	prop   ai.FixProposal
	anchor *models.BlockAnchor
	key    fixContentKey
}

// fixTargetOf takes the anchor the proposal carries (taken on the document it was made on); a proposal without one
// gets its block's anchor in the first of docs that has the block.
func fixTargetOf(prop ai.FixProposal, docs ...*models.FlowDocument) fixTarget {
	anchor := prop.Anchor
	for _, d := range docs {
		if anchor != nil {
			break
		}
		anchor = d.AnchorOf(prop.BlockID)
	}
	key := fixContentKey{RuleID: prop.RuleID, Subject: proposalSubject(prop), Name: prop.BlockLabel}
	if anchor != nil {
		key.Subflow, key.RawType, key.Name = anchor.Subflow, anchor.RawType, anchor.Name
	}
	return fixTarget{prop: prop, anchor: anchor, key: key}
}

// locate finds the target's finding in report (an analysis of doc), skipping and then marking consumed indices:
// the finding on the proposal's own block id while the flow was not re-parsed; else the one on the block at the
// anchor's place, where a block still standing without it means the finding is gone (never a sibling's); else, the
// anchor no longer resolving, the first finding with the content key. nil: the finding is gone.
func (t fixTarget) locate(report *models.AnalysisReport, doc *models.FlowDocument, consumed map[int]bool) *models.Finding {
	match := func(onBlock func(f *models.Finding) bool) *models.Finding {
		for j := range report.Findings {
			f := &report.Findings[j]
			if consumed[j] || f.RuleID != t.prop.RuleID || !onBlock(f) {
				continue
			}
			consumed[j] = true
			return f
		}
		return nil
	}
	subject := t.key.Subject
	if doc.BlocksByID[t.prop.BlockID] != nil {
		return match(func(f *models.Finding) bool { return f.BlockID == t.prop.BlockID && findingSubject(f) == subject })
	}
	if t.anchor != nil {
		if id := doc.ResolveAnchor(*t.anchor); id != "" {
			return match(func(f *models.Finding) bool { return f.BlockID == id && findingSubject(f) == subject })
		}
	}
	return match(func(f *models.Finding) bool { return fixContentKeyOf(f.RuleID, f, doc) == t.key })
}

// batchItemOutcome is one fix's verified result inside a batch; ID is the request item's (fixItemID).
type batchItemOutcome struct {
	ID      string
	RuleID  string
	Status  string // "applied" | "applied-unresolved" | "error" | "already-resolved" | "skipped"
	Message string
}

// batchOutcome aggregates a batch: "applied" when every item resolved cleanly, "applied-unresolved" when some
// did not, "error" when nothing applied.
type batchOutcome struct {
	Status string
	Detail string
	Items  []batchItemOutcome
	Final  *models.FlowDocument
}

// appliedCount is how many items of the batch were written.
func (o batchOutcome) appliedCount() int {
	n := 0
	for _, it := range o.Items {
		if it.Status == "applied" || it.Status == "applied-unresolved" {
			n++
		}
	}
	return n
}

func (o batchOutcome) ModelSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Batch result: %s.", o.Status)
	for _, it := range o.Items {
		fmt.Fprintf(&b, "\n- %s: %s", it.RuleID, it.Status)
		if it.Message != "" {
			fmt.Fprintf(&b, " (%s)", it.Message)
		}
	}
	if o.Final != nil {
		b.WriteString("\nThe flow was re-parsed: ALL block IDs have changed — call list_findings again before referencing any block.")
	}
	return scrubber.ScrubText(b.String())
}

// applyApprovedFixes applies an approved batch against the REAL flow one fix at a time: each apply re-parses
// (new block IDs), so each target is found again on the flow as it is when its turn comes (fixTarget.locate). An
// item whose write hits a version conflict is retried once on the flow as it is then. Per-item failures don't
// abort the batch; the final analysis verifies every item. The final document is returned for A1. As for a single
// fix, an error is returned only when ctx ended. stop, when set, is asked before each fix: a non-empty answer (a
// stop asked for during the batch) leaves that fix and the rest not attempted, with it as their message.
func (s *ChatService) applyApprovedFixes(ctx context.Context, snapshot *models.FlowDocument, props []ai.FixProposal, stop func() string) (batchOutcome, *models.FlowDocument, error) {
	out := batchOutcome{Status: "error", Items: make([]batchItemOutcome, 0, len(props))}
	fail := func(err error) (batchOutcome, *models.FlowDocument, error) {
		o, _, ferr := s.fixFailure(ctx, err)
		out.Detail = o.Detail
		return out, nil, ferr
	}
	if snapshot == nil {
		out.Detail = "no flow is loaded — nothing was changed"
		return out, nil, nil
	}
	if err := requireFixWriteScope(ctx); err != nil {
		return fail(err)
	}
	doc, err := s.GetAuthorizedFlow(ctx, snapshot.ID, FlowEdit)
	if err != nil {
		return fail(err)
	}
	// A nil analysis cache must bail gracefully (a call through it panics inside the apply window).
	if s.analysisCache == nil {
		out.Detail = "analysis not available — could not prepare the batch"
		return out, nil, nil
	}

	RecordFlowOp("chat_apply_fixes")

	targets := make([]fixTarget, len(props))
	for i, p := range props {
		targets[i] = fixTargetOf(p, snapshot, doc)
	}
	appliedFlags := make([]bool, len(props))

	var anyApplied, anyUnresolvedOrErr, unverified bool
	// outOfTime: the apply window's deadline ended the batch; later items are reported as not attempted.
	outOfTime := func() bool { return errors.Is(ctx.Err(), context.DeadlineExceeded) }
	const notAttemptedInTime = "not attempted: the batch ran out of time — nothing was changed for this fix"
	// B1.11: analysis is only re-run when the doc CHANGED since the last one, and the last mid-loop analysis
	// doubles as the verification report when nothing applied after it — a 20-item batch with k failures used to
	// run k+1 redundant full 41-rule walks.
	var curReport *models.AnalysisReport
	for i := range targets {
		prop := targets[i].prop
		if outOfTime() {
			out.Items = append(out.Items, batchItemOutcome{RuleID: prop.RuleID, Status: "error", Message: notAttemptedInTime})
			anyUnresolvedOrErr = true
			continue
		}
		if stop != nil {
			if why := stop(); why != "" {
				out.Items = append(out.Items, batchItemOutcome{RuleID: prop.RuleID, Status: "error", Message: why})
				anyUnresolvedOrErr = true
				continue
			}
		}
		var item *batchItemOutcome
		var updated *models.FlowDocument
		for attempt := 1; ; attempt++ {
			if curReport == nil {
				r, aerr := s.analysisCache.AnalyzeFlowReadOnly(ctx, doc)
				if aerr != nil || r == nil {
					msg := "re-analysis failed mid-batch"
					if outOfTime() {
						msg = notAttemptedInTime
					}
					item = &batchItemOutcome{RuleID: prop.RuleID, Status: "error", Message: msg}
					break
				}
				curReport = r
			}
			// The findings of the targets before this one are theirs, so identical siblings stay one-to-one.
			consumed := map[int]bool{}
			for k := 0; k < i; k++ {
				targets[k].locate(curReport, doc, consumed)
			}
			f := targets[i].locate(curReport, doc, consumed)
			if f == nil {
				// Gone before we touched it: an earlier fix cascaded it away, or a concurrent edit removed it.
				item = &batchItemOutcome{RuleID: prop.RuleID, Status: "already-resolved", Message: "the finding no longer appears on the flow"}
				break
			}
			var ferr error
			updated, ferr = s.applyFixHook()(ctx, doc, f.BlockID, prop.FixType, prop.RuleID, prop.Variable, prop.Property)
			if errors.Is(ferr, storageif.ErrVersionConflict) && attempt < fixConflictAttempts {
				// Someone saved the flow since this batch read it: once more on the flow as it is now.
				fresh, gerr := s.GetAuthorizedFlow(ctx, snapshot.ID, FlowEdit)
				if gerr == nil {
					doc, curReport = fresh, nil
					continue
				}
				ferr = gerr
			}
			if ferr != nil || updated == nil {
				msg := "the fix could not be applied"
				if ferr != nil {
					logger.FromContext(ctx).Warn("chat: an approved batch fix was not applied", "rule", prop.RuleID, "error", ferr)
					msg = fixFailureDetail(ferr)
				}
				// Relabel only a failure the window caused, not a quick error of its own that returned after it.
				if outOfTime() && (ferr == nil || errors.Is(ferr, context.DeadlineExceeded)) {
					msg = "the batch ran out of time during this fix — check whether it changed the flow"
				}
				item = &batchItemOutcome{RuleID: prop.RuleID, Status: "error", Message: msg}
			}
			break
		}
		if item != nil {
			out.Items = append(out.Items, *item)
			if item.Status == "error" {
				anyUnresolvedOrErr = true
			}
			continue
		}
		doc = updated
		// The applied fix re-parsed the doc: the report (and its block IDs) are stale — force re-analysis.
		curReport = nil
		appliedFlags[i] = true
		anyApplied = true
		RecordFlowOp("chat_apply_fix")
		// Placeholder keeps out.Items index-aligned with targets; the verification pass fills it.
		out.Items = append(out.Items, batchItemOutcome{RuleID: prop.RuleID})
	}

	// Final verification: an applied item whose finding can still be found did not actually resolve.
	if anyApplied {
		// B1.11: reuse the last mid-loop analysis when no apply staled it.
		fin := curReport
		if fin == nil {
			var ferr error
			fin, ferr = s.analysisCache.AnalyzeFlowReadOnly(ctx, doc)
			if ferr != nil {
				fin = nil
			}
		}
		if fin != nil {
			// The items not applied claim their findings first, so an applied item is never verified against a
			// sibling's.
			consumed := map[int]bool{}
			for i := range targets {
				if !appliedFlags[i] {
					targets[i].locate(fin, doc, consumed)
				}
			}
			for i := range targets {
				if !appliedFlags[i] {
					continue
				}
				if targets[i].locate(fin, doc, consumed) != nil {
					out.Items[i] = batchItemOutcome{RuleID: targets[i].prop.RuleID, Status: "applied-unresolved", Message: "the finding still appears after re-analysis"}
					anyUnresolvedOrErr = true
					continue
				}
				out.Items[i] = batchItemOutcome{RuleID: targets[i].prop.RuleID, Status: "applied"}
			}
		} else {
			for i := range targets {
				if !appliedFlags[i] || out.Items[i].Status != "" {
					continue
				}
				if outOfTime() {
					// Applied; only the check ran out of time — not evidence the fix failed.
					out.Items[i] = batchItemOutcome{RuleID: targets[i].prop.RuleID, Status: "applied", Message: "verification skipped: re-analysis ran out of time"}
					unverified = true
					continue
				}
				out.Items[i] = batchItemOutcome{RuleID: targets[i].prop.RuleID, Status: "applied-unresolved", Message: "final re-analysis failed; could not verify"}
				anyUnresolvedOrErr = true
			}
		}
	}
	// Fill statuses for skipped items ("already-resolved" already set above); out.Items[i] is targets[i]'s (one
	// outcome appended per item, in order), so the request item's id is stamped here for all of them.
	for i := range out.Items {
		out.Items[i].ID = fixItemID(targets[i].prop)
		if out.Items[i].Status == "" {
			out.Items[i].Status = "error"
			out.Items[i].Message = "not attempted"
			anyUnresolvedOrErr = true
		}
	}

	s.InvalidateChatContext(doc.ID)

	switch {
	case !anyApplied && !anyUnresolvedOrErr:
		out.Status = "applied" // everything was already resolved — nothing to do
		out.Detail = "every finding in the batch no longer appears on the flow"
	case !anyApplied:
		out.Status = "error"
		out.Detail = "no fix in the batch could be applied"
	default:
		if anyUnresolvedOrErr {
			out.Status = "applied-unresolved"
			out.Detail = "the batch was applied with some items needing review"
		} else {
			out.Status = "applied"
			out.Detail = "all fixes applied and verified"
			if unverified {
				out.Detail = "all fixes applied; verification was skipped because re-analysis ran out of time"
			}
		}
	}
	logger.FromContext(ctx).Info("chat-driven batch fix applied", "flow", doc.ID, "items", len(props), "status", out.Status)
	out.Final = doc
	return out, doc, nil
}
