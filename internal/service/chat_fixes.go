package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
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

// ApplyFixWithApproval implements ai.ToolFixApplier: the returned string is always model-readable on nil-error
// paths; an error means the stream context died and the loop's failure handling takes over.
func (a *chatFixApplier) ApplyFixWithApproval(ctx context.Context, prop ai.FixProposal) (string, error) {
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
	dec, ended := a.svc.requestApproval(ctx, a.streamID, approvalRequestFor(fixToolSpec("apply_fix"), prop.ProposalID, []map[string]interface{}{item}, flat), a.emit, a.touch)
	switch ended {
	case approvalCancelled:
		return "", ctx.Err()
	case approvalTimedOut:
		return "the user did not respond to the approval prompt in time — nothing was changed. Ask them whether to try again or apply it manually.", nil
	}
	if !dec.approved {
		a.emitFixDecision(prop.ProposalID, "declined", "")
		return "the user DECLINED this fix — nothing was changed. Do not call apply_fix again for the same finding unless the user explicitly asks; explain the change instead.", nil
	}

	a.emitFixDecision(prop.ProposalID, "applying", "")
	// Keepalive through the apply+verify window (L4): no provider chunks flow then — a >90s re-analysis would
	// otherwise trip the idle watchdog even though fixApplyToolTimeout allows 120s. Closed via defer so a panic
	// in the apply path cannot strand the goroutine.
	applyDone := a.beginApplyKeepalive(ctx)
	defer close(applyDone)
	outcome, updated, err := a.svc.applyApprovedFix(ctx, a.doc, prop)
	if err != nil {
		a.emitFixDecision(prop.ProposalID, "error", err.Error())
		return "", err
	}
	// A1: hand the fresh post-fix document to the owning loop BEFORE the decision event, so the model's next
	// tool call runs against the fixed flow (new block IDs included).
	note := a.handOver(updated)
	a.emitFixDecision(prop.ProposalID, outcome.Status, outcome.Detail)
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
			logger.Error("chat: the post-fix flow's redacting clone could not be made; tools keep the previous clone", "error", err)
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
func (a *chatFixApplier) ApplyFixesWithApproval(ctx context.Context, props []ai.FixProposal) (string, error) {
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
	dec, ended := a.svc.requestApproval(ctx, a.streamID, approvalRequestFor(fixToolSpec("apply_fixes"), batchID, items,
		map[string]interface{}{"batch": true, "count": len(props)}), a.emit, a.touch)
	switch ended {
	case approvalCancelled:
		return "", ctx.Err()
	case approvalTimedOut:
		return "the user did not respond to the batch approval in time — nothing was changed. Ask whether to try again.", nil
	}
	// excluded is the winning decision's per-item opt-outs, received with it.
	excluded := dec.excluded
	if !dec.approved {
		a.emitFixDecision(batchID, "declined", "")
		return "the user DECLINED the batch — nothing was changed. Do not re-propose the same batch unless asked; explain the changes instead.", nil
	}

	// Per-item opt-out (U4.1): apply only the items the user left checked (indices == props order).
	if len(excluded) > 0 {
		skip := make(map[int]bool, len(excluded))
		for _, i := range excluded {
			if i >= 0 && i < len(props) {
				skip[i] = true
			}
		}
		if len(skip) > 0 {
			kept := props[:0]
			for i, p := range props {
				if !skip[i] {
					kept = append(kept, p)
				}
			}
			props = kept
			if len(props) == 0 {
				a.emitFixDecision(batchID, "declined", "every fix in the batch was deselected")
				return "the user deselected every fix in the batch — nothing was changed.", nil
			}
		}
	}

	a.emitFixDecision(batchID, "applying", "")
	// Keepalive through the whole multi-apply window (see ApplyFixWithApproval); closed via defer.
	applyDone := a.beginApplyKeepalive(ctx)
	defer close(applyDone)
	outcome, updated, err := a.svc.applyApprovedFixes(ctx, a.doc, props)
	if err != nil {
		a.emitFixDecision(batchID, "error", err.Error())
		return "", err
	}
	note := a.handOver(updated)
	a.emitBatchDecision(batchID, outcome)
	return outcome.ModelSummary() + a.followGrounded(updated) + note, nil
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
	Status string // "applied" | "applied-unresolved" | "error"
	Detail string
}

// ModelSummary renders the outcome for the model's next turn. Scrubbed: the
// detail can carry apply-path error text, and model-visible strings never
// bypass the scrubber (chat messages, tool results, and summaries all pass
// through it; the provider boundary is not re-scrubbing here).
func (o chatFixOutcome) ModelSummary() string {
	switch o.Status {
	case "applied":
		return scrubber.ScrubText("APPLIED and verified: " + o.Detail)
	case "applied-unresolved":
		return scrubber.ScrubText("APPLIED, but " + o.Detail)
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

// applyApprovedFix applies an approved proposal against the REAL flow and verifies it: re-resolves a fresh
// document the stream's actor may edit (FlowEdit; the stream itself only required ChatUse), runs the fix (patch
// recomputed on the unscrubbed source), invalidates the cached chat context, and re-analyzes to check whether the
// finding (by its re-parse-stable fingerprint) disappeared. The updated document is returned for the caller's
// refresh (A1).
//
// The apply+verify window is keepalive-touched: without touches the idle watchdog would cancel a healthy stream
// during a >90s re-analysis even though fixApplyToolTimeout allows 120s.
func (s *ChatService) applyApprovedFix(ctx context.Context, snapshot *models.FlowDocument, prop ai.FixProposal) (chatFixOutcome, *models.FlowDocument, error) {
	if snapshot == nil {
		return chatFixOutcome{Status: "error", Detail: "no flow loaded"}, nil, fmt.Errorf("no flow loaded")
	}
	// Re-checked at apply time (defense in depth): this is the mutation itself.
	if err := requireFixWriteScope(ctx); err != nil {
		return chatFixOutcome{Status: "error", Detail: err.Error()}, nil, err
	}
	// Fresh doc + FlowEdit check at apply time: the stream was authorized for ChatUse and the snapshot may be stale.
	doc, err := s.GetAuthorizedFlow(ctx, snapshot.ID, FlowEdit)
	if err != nil {
		return chatFixOutcome{Status: "error", Detail: "not allowed to edit this flow: " + err.Error()}, nil, err
	}

	RecordFlowOp("chat_apply_fix")
	updated, err := s.applyFixHook()(ctx, doc, prop.BlockID, prop.FixType, prop.RuleID, prop.Variable, prop.Property)
	if err != nil {
		return chatFixOutcome{Status: "error", Detail: err.Error()}, nil, err
	}

	// The flow changed under the chat's cached (scrubbed) context — drop it for the fixed source.
	s.InvalidateChatContext(doc.ID)

	if updated == nil || s.analysisCache == nil {
		return chatFixOutcome{
			Status: "applied-unresolved",
			Detail: "the fix was applied, but the result could not be re-analyzed to verify it",
		}, updated, nil
	}
	report, err := s.analysisCache.AnalyzeFlow(ctx, updated)
	if err != nil || report == nil {
		return chatFixOutcome{
			Status: "applied-unresolved",
			Detail: "the fix was applied, but re-analysis failed so it could not be verified",
		}, updated, nil
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
		}, updated, nil
	}
	logger.Info("chat-driven fix applied", "flow", doc.ID, "rule", prop.RuleID, "fix", prop.FixType)
	detail := fmt.Sprintf("the finding no longer appears after re-analysis (flow now has %d findings)", len(report.Findings))
	if updated != nil {
		// The apply re-parsed the source, minting fresh block IDs: every ID the model saw is stale, so say so
		// explicitly — otherwise its next apply_fix targets a dead ID.
		detail += ". The flow was re-parsed: ALL block IDs have changed — call list_findings again before referencing any block"
	}
	return chatFixOutcome{
		Status: "applied",
		Detail: detail,
	}, updated, nil
}

// fixContentKey re-associates a batch target across sequential applies: a patch shifts line numbers and the
// re-parse mints fresh block IDs + fingerprints, so neither survives; the block's identity content does.
// Truly identical sibling blocks share a key — items consume matches in order, which may swap siblings, but
// identical siblings get identical fixes.
type fixContentKey struct {
	RuleID, Subflow, RawType, Name, Subject string
}

func fixContentKeyOf(ruleID string, f *models.Finding, doc *models.FlowDocument) fixContentKey {
	k := fixContentKey{RuleID: ruleID}
	if doc == nil {
		return k
	}
	if b := doc.BlocksByID[f.BlockID]; b != nil {
		k.RawType, k.Name = b.RawType, b.Name
		if sf := doc.BlockSubflow[f.BlockID]; sf != nil {
			k.Subflow = sf.Name
		}
	}
	if v, ok := f.Metadata["property"].(string); ok && v != "" {
		k.Subject = "property:" + v
	} else if v, ok := f.Metadata["variable"].(string); ok && v != "" {
		k.Subject = "variable:" + v
	}
	return k
}

// batchItemOutcome is one fix's verified result inside a batch; ID is the request item's (fixItemID).
type batchItemOutcome struct {
	ID      string
	RuleID  string
	Status  string // "applied" | "applied-unresolved" | "error" | "already-resolved"
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
// (new block IDs), so the next target is re-associated on the fresh analysis by content key. Per-item failures
// don't abort the batch; the final analysis verifies every item. The final document is returned for A1.
func (s *ChatService) applyApprovedFixes(ctx context.Context, snapshot *models.FlowDocument, props []ai.FixProposal) (batchOutcome, *models.FlowDocument, error) {
	out := batchOutcome{Status: "error", Items: make([]batchItemOutcome, 0, len(props))}
	if snapshot == nil {
		out.Detail = "no flow loaded"
		return out, nil, fmt.Errorf("no flow loaded")
	}
	if err := requireFixWriteScope(ctx); err != nil {
		out.Detail = err.Error()
		return out, nil, err
	}
	doc, err := s.GetAuthorizedFlow(ctx, snapshot.ID, FlowEdit)
	if err != nil {
		out.Detail = "not allowed to edit this flow: " + err.Error()
		return out, nil, err
	}

	RecordFlowOp("chat_apply_fixes")

	type pending struct {
		prop ai.FixProposal
		key  fixContentKey
	}
	pend := make([]pending, len(props))
	appliedFlags := make([]bool, len(props))

	// Associate every target with the CURRENT doc's analysis: exact BlockID first; a miss means a concurrent
	// edit — the content key still gives a chance. A nil analysis cache must bail gracefully (a call through it
	// panics inside the apply window).
	if s.analysisCache == nil {
		out.Detail = "analysis not available — could not prepare the batch"
		return out, nil, nil
	}
	initReport, err := s.analysisCache.AnalyzeFlowReadOnly(ctx, doc)
	if err != nil || initReport == nil {
		out.Detail = "analysis not available — could not prepare the batch"
		return out, nil, err
	}
	for i, p := range props {
		pend[i] = pending{prop: p}
		for j := range initReport.Findings {
			f := &initReport.Findings[j]
			if f.BlockID == p.BlockID && f.RuleID == p.RuleID {
				pend[i].key = fixContentKeyOf(p.RuleID, f, doc)
				break
			}
		}
		if pend[i].key == (fixContentKey{}) {
			pend[i].key = fixContentKey{RuleID: p.RuleID, Name: p.BlockLabel}
		}
	}

	// findFinding locates the not-yet-consumed finding matching key, preferring an exact BlockID+Rule hit.
	findFinding := func(report *models.AnalysisReport, d *models.FlowDocument, key fixContentKey, prop ai.FixProposal, consumed map[int]bool) *models.Finding {
		for j := range report.Findings {
			if consumed[j] {
				continue
			}
			f := &report.Findings[j]
			if f.RuleID == prop.RuleID && f.BlockID == prop.BlockID {
				consumed[j] = true
				return f
			}
		}
		for j := range report.Findings {
			if consumed[j] {
				continue
			}
			f := &report.Findings[j]
			if fixContentKeyOf(f.RuleID, f, d) == key {
				consumed[j] = true
				return f
			}
		}
		return nil
	}

	var anyApplied, anyUnresolvedOrErr bool
	// B1.11: analysis is only re-run when the doc CHANGED since the last one, and the last mid-loop analysis
	// doubles as the verification report when nothing applied after it — a 20-item batch with k failures used to
	// run k+1 redundant full 41-rule walks.
	var curReport *models.AnalysisReport
	for i := range pend {
		if curReport == nil {
			r, aerr := s.analysisCache.AnalyzeFlowReadOnly(ctx, doc)
			if aerr != nil || r == nil {
				out.Items = append(out.Items, batchItemOutcome{RuleID: pend[i].prop.RuleID, Status: "error", Message: "re-analysis failed mid-batch"})
				anyUnresolvedOrErr = true
				continue
			}
			curReport = r
		}
		report := curReport
		consumed := map[int]bool{}
		// Mark findings consumed by EARLIER items of this same analysis pass, so identical-sibling association
		// stays one-to-one.
		for k := 0; k < i; k++ {
			for j := range report.Findings {
				f := &report.Findings[j]
				if !consumed[j] && fixContentKeyOf(f.RuleID, f, doc) == pend[k].key && f.RuleID == pend[k].prop.RuleID {
					consumed[j] = true
					break
				}
			}
		}
		f := findFinding(report, doc, pend[i].key, pend[i].prop, consumed)
		if f == nil {
			// Gone before we touched it: an earlier fix cascaded it away, or a concurrent edit removed it.
			out.Items = append(out.Items, batchItemOutcome{RuleID: pend[i].prop.RuleID, Status: "already-resolved", Message: "the finding no longer appears on the flow"})
			continue
		}
		updated, ferr := s.applyFixHook()(ctx, doc, f.BlockID, pend[i].prop.FixType, pend[i].prop.RuleID, pend[i].prop.Variable, pend[i].prop.Property)
		if ferr != nil || updated == nil {
			msg := "apply failed"
			if ferr != nil {
				msg = ferr.Error()
			}
			out.Items = append(out.Items, batchItemOutcome{RuleID: pend[i].prop.RuleID, Status: "error", Message: msg})
			anyUnresolvedOrErr = true
			continue
		}
		doc = updated
		// The applied fix re-parsed the doc: the report (and its block IDs) are stale — force re-analysis.
		curReport = nil
		appliedFlags[i] = true
		anyApplied = true
		RecordFlowOp("chat_apply_fix")
		// Placeholder keeps out.Items index-aligned with pend; the verification pass fills it.
		out.Items = append(out.Items, batchItemOutcome{RuleID: pend[i].prop.RuleID})
	}

	// Final verification: an applied item whose finding (by content key) still exists did not actually resolve.
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
			for i := range pend {
				if !appliedFlags[i] {
					continue
				}
				for j := range fin.Findings {
					if fixContentKeyOf(fin.Findings[j].RuleID, &fin.Findings[j], doc) == pend[i].key {
						out.Items[i].Status = "applied-unresolved"
						out.Items[i].Message = "the finding still appears after re-analysis"
						anyUnresolvedOrErr = true
						break
					}
				}
				if out.Items[i].Status == "" {
					out.Items[i] = batchItemOutcome{RuleID: pend[i].prop.RuleID, Status: "applied"}
				}
			}
		} else {
			for i := range pend {
				if appliedFlags[i] && out.Items[i].Status == "" {
					out.Items[i] = batchItemOutcome{RuleID: pend[i].prop.RuleID, Status: "applied-unresolved", Message: "final re-analysis failed; could not verify"}
					anyUnresolvedOrErr = true
				}
			}
		}
	}
	// Fill statuses for skipped items ("already-resolved" already set above); out.Items[i] is pend[i]'s (one
	// outcome appended per item, in order), so the request item's id is stamped here for all of them.
	for i := range out.Items {
		out.Items[i].ID = fixItemID(pend[i].prop)
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
		}
	}
	logger.Info("chat-driven batch fix applied", "flow", doc.ID, "items", len(props), "status", out.Status)
	out.Final = doc
	return out, doc, nil
}
