package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/metrics"
	"pad-core/ai/scrubber"
	"pad-core/logger"
	"pad-core/models"

	"github.com/google/uuid"
)

// fixDecisionTimeout bounds the apply_fix approval wait. Mirrors
// ai.FixDecisionTimeout (kept as a var so tests can shorten it via
// ChatService.fixDecisionWait).
var fixDecisionTimeout = ai.FixDecisionTimeout

// decisionWait resolves the effective approval-wait duration (test override
// aware).
func (s *ChatService) decisionWait() time.Duration {
	if s.fixDecisionWait > 0 {
		return s.fixDecisionWait
	}
	return fixDecisionTimeout
}

// fixDecisionKeepalive is how often the approval wait touches the stream's
// activity clock. Without it the idle watchdog (90s, counting only provider
// chunks + explicit touches) could cancel a healthy stream while the user is
// still reading the proposal.
const fixDecisionKeepalive = 15 * time.Second

// ErrNoPendingFix is returned by ResolveFixDecision when the proposal ID is
// unknown, already decided, or belongs to a different stream. Exported so the
// HTTP layer can map it to a distinct status.
var ErrNoPendingFix = errors.New("no pending fix proposal for this stream")

// fixDecisionState is one registered approval prompt. decided is buffered so
// ResolveFixDecision never blocks and the FIRST decision wins (later sends
// find it full and are dropped).
type fixDecisionState struct {
	streamID string
	decided  chan bool
	// excludedBatchIdx carries per-item OPT-OUTS for a batch approval
	// (U4.1): indices into the card's item list the user deselected. Written
	// BEFORE the channel send (ResolveFixDecision) and read AFTER the receive
	// (the batch consumer) — the channel gives the happens-before edge.
	excludedBatchIdx []int
}

// chatFixApplier implements ai.ToolFixApplier for one chat stream: it emits
// the fix_proposal SSE event to the stream's owner, waits for their decision
// (via ChatService.ResolveFixDecision), and on approval applies the fix to the
// REAL flow through FlowService.ApplyFix (the patch is re-computed server-side
// against the unscrubbed source — the scrubbed doc only ever produced the
// preview), re-analyzes, and reports whether the finding actually resolved.
type chatFixApplier struct {
	svc      *ChatService
	scope    string
	doc      *models.FlowDocument // pre-fix snapshot: used for flow identity/authorization
	streamID string
	emit     func(eventType string, data map[string]interface{})
	touch    func()
	// onDocReplaced, when set, is invoked with the fresh post-fix document so
	// the owning tool loop can swap its working copy mid-stream (A1) — the
	// re-parse mints new block IDs, and follow-up tools on the stale snapshot
	// would mis-answer or fail.
	onDocReplaced func(fresh *models.FlowDocument)
}

// newFixApplier builds the per-stream applier. Returns nil when the service
// can't apply fixes (no flow service wired — bare-struct tests), leaving
// tctx.Fixes nil and apply_fix to report "not available".
func (s *ChatService) newFixApplier(scope string, doc *models.FlowDocument, streamID string, emit func(string, map[string]interface{}), touch func(), onDocReplaced func(*models.FlowDocument)) ai.ToolFixApplier {
	if s.flowCache == nil || emit == nil {
		return nil
	}
	return &chatFixApplier{svc: s, scope: scope, doc: doc, streamID: streamID, emit: emit, touch: touch, onDocReplaced: onDocReplaced}
}

// ApplyFixWithApproval implements ai.ToolFixApplier. The returned string is
// always model-readable on the nil-error paths (approved/applied,
// applied-unverified, declined, timed out); an error means the stream context
// died (cancelled/timed out) and the loop's own failure handling takes over.
func (a *chatFixApplier) ApplyFixWithApproval(ctx context.Context, prop ai.FixProposal) (string, error) {
	if a.touch != nil {
		a.touch()
	}
	st := &fixDecisionState{streamID: a.streamID, decided: make(chan bool, 1)}
	a.svc.pendingFixDecisions.Store(prop.ProposalID, st)
	defer a.svc.pendingFixDecisions.Delete(prop.ProposalID)

	a.emit("fix_proposal", map[string]interface{}{
		"proposalId": prop.ProposalID,
		"ruleId":     prop.RuleID,
		"fixType":    prop.FixType,
		"blockId":    prop.BlockID,
		// Block names are never masked by ScrubDocument (only Properties) —
		// scrub the client-facing label as defense in depth.
		"blockLabel": scrubber.ScrubText(prop.BlockLabel),
		"line":       prop.Line,
		"summary":    prop.Summary,
	})

	var approved bool
	var timedOut bool
	timeout := time.NewTimer(a.svc.decisionWait())
	defer timeout.Stop()
	keepalive := time.NewTicker(fixDecisionKeepalive)
	defer keepalive.Stop()
wait:
	for {
		select {
		case approved = <-st.decided:
			break wait
		case <-keepalive.C:
			if a.touch != nil {
				a.touch()
			}
		case <-timeout.C:
			timedOut = true
			break wait
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	if timedOut {
		a.emitFixDecision(prop.ProposalID, "timeout", "")
		return "the user did not respond to the approval prompt in time — nothing was changed. Ask them whether to try again or apply it manually.", nil
	}
	if !approved {
		a.emitFixDecision(prop.ProposalID, "declined", "")
		return "the user DECLINED this fix — nothing was changed. Do not call apply_fix again for the same finding unless the user explicitly asks; explain the change instead.", nil
	}

	a.emitFixDecision(prop.ProposalID, "applying", "")
	// Keepalive through the apply+verify window (L4): no provider chunks flow
	// while it runs — a >90s re-analysis on a large flow would otherwise trip
	// the idle watchdog even though fixApplyToolTimeout allows 120s.
	applyDone := make(chan struct{})
	if a.touch != nil {
		keepalive := time.NewTicker(fixDecisionKeepalive)
		go func() {
			for {
				select {
				case <-keepalive.C:
					a.touch()
				case <-applyDone:
					keepalive.Stop()
					return
				}
			}
		}()
	}
	outcome, updated, err := a.svc.applyApprovedFix(ctx, a.scope, a.doc, prop)
	close(applyDone)
	if err != nil {
		a.emitFixDecision(prop.ProposalID, "error", err.Error())
		return "", err
	}
	// A1: hand the fresh post-fix document to the owning loop BEFORE the
	// decision event, so any tool call the model makes next iteration runs
	// against the fixed flow (new block IDs included).
	if updated != nil && a.onDocReplaced != nil {
		a.onDocReplaced(updated)
	}
	a.emitFixDecision(prop.ProposalID, outcome.Status, outcome.Detail)
	return outcome.ModelSummary(), nil
}

func (a *chatFixApplier) emitFixDecision(proposalID, status, message string) {
	metrics.RecordChatFixDecision(status)
	data := map[string]interface{}{"proposalId": proposalID, "status": status}
	if message != "" {
		data["message"] = message
	}
	a.emit("fix_decision", data)
}

// ApplyFixesWithApproval implements the batch half of ai.ToolFixApplier: ONE
// approval prompt carrying every preview, one decision, then a sequential
// apply with per-item verification. See applyApprovedFixes for the loop.
func (a *chatFixApplier) ApplyFixesWithApproval(ctx context.Context, props []ai.FixProposal) (string, error) {
	if len(props) == 0 {
		return "error: empty batch", nil
	}
	if a.touch != nil {
		a.touch()
	}
	batchID := props[0].ProposalID + "-batch-" + uuid.NewString()[:8]
	st := &fixDecisionState{streamID: a.streamID, decided: make(chan bool, 1)}
	a.svc.pendingFixDecisions.Store(batchID, st)
	defer a.svc.pendingFixDecisions.Delete(batchID)

	items := make([]map[string]interface{}, len(props))
	for i, p := range props {
		items[i] = map[string]interface{}{
			"ruleId":     p.RuleID,
			"fixType":    p.FixType,
			"blockId":    p.BlockID,
			"blockLabel": scrubber.ScrubText(p.BlockLabel), // see single-proposal note
			"line":       p.Line,
			"summary":    p.Summary,
		}
	}
	a.emit("fix_proposal", map[string]interface{}{
		"proposalId": batchID,
		"batch":      true,
		"count":      len(props),
		"items":      items,
	})

	var approved bool
	var timedOut bool
	timeout := time.NewTimer(a.svc.decisionWait())
	defer timeout.Stop()
	keepalive := time.NewTicker(fixDecisionKeepalive)
	defer keepalive.Stop()
wait:
	for {
		select {
		case approved = <-st.decided:
			break wait
		case <-keepalive.C:
			if a.touch != nil {
				a.touch()
			}
		case <-timeout.C:
			timedOut = true
			break wait
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	if timedOut {
		a.emitFixDecision(batchID, "timeout", "")
		return "the user did not respond to the batch approval in time — nothing was changed. Ask whether to try again.", nil
	}
	if !approved {
		a.emitFixDecision(batchID, "declined", "")
		return "the user DECLINED the batch — nothing was changed. Do not re-propose the same batch unless asked; explain the changes instead.", nil
	}

	// Per-item opt-out (U4.1): apply only the items the user left checked.
	// Indices are positions in the emitted item list (== props order).
	if len(st.excludedBatchIdx) > 0 {
		skip := make(map[int]bool, len(st.excludedBatchIdx))
		for _, i := range st.excludedBatchIdx {
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
	// Keepalive through the whole multi-apply window (N applies + re-analyses
	// emit no provider chunks — see ApplyFixWithApproval).
	applyDone := make(chan struct{})
	if a.touch != nil {
		ka := time.NewTicker(fixDecisionKeepalive)
		go func() {
			for {
				select {
				case <-ka.C:
					a.touch()
				case <-applyDone:
					ka.Stop()
					return
				}
			}
		}()
	}
	outcome, updated, err := a.svc.applyApprovedFixes(ctx, a.scope, a.doc, props)
	close(applyDone)
	if err != nil {
		a.emitFixDecision(batchID, "error", err.Error())
		return "", err
	}
	if updated != nil && a.onDocReplaced != nil {
		a.onDocReplaced(updated)
	}
	a.emitBatchDecision(batchID, outcome)
	return outcome.ModelSummary(), nil
}

func (a *chatFixApplier) emitBatchDecision(batchID string, outcome batchOutcome) {
	metrics.RecordChatFixDecision(outcome.Status)
	data := map[string]interface{}{
		"proposalId": batchID,
		"status":     outcome.Status,
	}
	if outcome.Detail != "" {
		data["message"] = outcome.Detail
	}
	items := make([]map[string]interface{}, len(outcome.Items))
	for i, it := range outcome.Items {
		m := map[string]interface{}{"ruleId": it.RuleID, "status": it.Status}
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

// ModelSummary renders the outcome for the model's next turn.
func (o chatFixOutcome) ModelSummary() string {
	switch o.Status {
	case "applied":
		return "APPLIED and verified: " + o.Detail
	case "applied-unresolved":
		return "APPLIED, but " + o.Detail
	default:
		return "apply failed: " + o.Detail
	}
}

// applyFixFunc is the FlowService-backed mutation hook. A field (rather than a
// direct call) so service tests can substitute it, mirroring the
// watchdogInterval/idleTimeout test overrides.
type applyFixFunc func(ctx context.Context, doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (*models.FlowDocument, error)

func (s *ChatService) applyFixHook() applyFixFunc {
	if s.applyFixFunc != nil {
		return s.applyFixFunc
	}
	return func(ctx context.Context, doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (*models.FlowDocument, error) {
		return s.flowCache.ApplyFix(ctx, doc, blockID, fixType, ruleID, variable, property)
	}
}

// applyApprovedFix applies an approved proposal against the REAL flow and
// verifies it: re-resolves a fresh document with EDITOR permission (the chat
// stream itself only required viewer), runs the fix (patch recomputed
// server-side on the unscrubbed source), invalidates the cached chat context
// for the changed flow, and re-analyzes to check whether the finding (by its
// re-parse-stable fingerprint) actually disappeared. The updated document is
// returned alongside the outcome so the caller can refresh its working copy
// (A1); it is nil exactly when the apply/analysis paths could not produce one.
//
// The whole apply+verify window is keepalive-touched: no provider chunks flow
// while it runs, so without touches the idle watchdog (>90s on a large flow's
// re-analysis) would cancel a healthy stream even though fixApplyToolTimeout
// allows 120s.
func (s *ChatService) applyApprovedFix(ctx context.Context, scope string, snapshot *models.FlowDocument, prop ai.FixProposal) (chatFixOutcome, *models.FlowDocument, error) {
	if snapshot == nil {
		return chatFixOutcome{Status: "error", Detail: "no flow loaded"}, nil, fmt.Errorf("no flow loaded")
	}
	// Fresh doc + editor check at apply time: the stream was authorized as
	// viewer, and the snapshot may be stale mid-conversation. In local mode
	// GetAuthorized resolves the current doc without authz.
	doc, err := s.GetAuthorizedFlow(ctx, snapshot.ID, scope, "editor")
	if err != nil {
		return chatFixOutcome{Status: "error", Detail: "not allowed to edit this flow: " + err.Error()}, nil, err
	}

	metrics.RecordFlowOp("chat_apply_fix")
	updated, err := s.applyFixHook()(ctx, doc, prop.BlockID, prop.FixType, prop.RuleID, prop.Variable, prop.Property)
	if err != nil {
		return chatFixOutcome{Status: "error", Detail: err.Error()}, nil, err
	}

	// The flow changed under the chat's cached (scrubbed) context — drop it so
	// the next turn rebuilds against the fixed source.
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

	// Verify by fingerprint: the re-parse mints fresh block IDs, so BlockID
	// comparisons would always "resolve". Fingerprints are content-stable
	// across re-parses by design. An empty fingerprint (older report) falls
	// back to counting rule findings on the re-parsed block set.
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
		// The apply re-parses the source, minting fresh block IDs: every block
		// ID the model saw earlier (including the one in this proposal) is now
		// stale. Say so explicitly — otherwise the model's next apply_fix
		// targets a dead ID and fails with an opaque "block not found".
		detail += ". The flow was re-parsed: ALL block IDs have changed — call list_findings again before referencing any block"
	}
	return chatFixOutcome{
		Status: "applied",
		Detail: detail,
	}, updated, nil
}

// ResolveFixDecision delivers the user's approve/decline for a pending
// proposal. First decision wins; the stream must own the proposal.
func (s *ChatService) ResolveFixDecision(streamID, proposalID string, approved bool, excludedBatchIdx []int) error {
	v, ok := s.pendingFixDecisions.Load(proposalID)
	if !ok {
		return ErrNoPendingFix
	}
	st := v.(*fixDecisionState)
	if st.streamID != streamID {
		return ErrNoPendingFix
	}
	if approved && len(excludedBatchIdx) > 0 {
		st.excludedBatchIdx = excludedBatchIdx
	}
	select {
	case st.decided <- approved:
	default: // already decided — first decision wins
	}
	return nil
}

// fixContentKey re-associates a batch target across sequential applies. A
// patch shifts line numbers and the re-parse mints fresh block IDs + (line-
// bearing) fingerprints, so neither survives; the block's identity content
// does. Ambiguity note: truly identical sibling blocks share a key — items
// consume matches in order, which may swap siblings, but identical siblings
// get identical fixes.
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

// batchItemOutcome is one fix's verified result inside a batch.
type batchItemOutcome struct {
	RuleID  string
	Status  string // "applied" | "applied-unresolved" | "error" | "already-resolved"
	Message string
}

// batchOutcome aggregates a batch: overall Status is "applied" when every item
// resolved cleanly, "applied-unresolved" when some did not (partial success,
// mixed errors), "error" when nothing applied at all.
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
	return b.String()
}

// applyApprovedFixes applies an approved batch against the REAL flow, one fix
// at a time: each apply re-parses (new block IDs), so the next target is
// re-associated on the fresh analysis by content key before its patch is
// computed. Per-item failures don't abort the batch. The final analysis
// verifies every item. The final document is returned for the loop's doc
// refresh (A1); nil when nothing could be applied at all.
func (s *ChatService) applyApprovedFixes(ctx context.Context, scope string, snapshot *models.FlowDocument, props []ai.FixProposal) (batchOutcome, *models.FlowDocument, error) {
	out := batchOutcome{Status: "error", Items: make([]batchItemOutcome, 0, len(props))}
	if snapshot == nil {
		out.Detail = "no flow loaded"
		return out, nil, fmt.Errorf("no flow loaded")
	}
	doc, err := s.GetAuthorizedFlow(ctx, snapshot.ID, scope, "editor")
	if err != nil {
		out.Detail = "not allowed to edit this flow: " + err.Error()
		return out, nil, err
	}

	metrics.RecordFlowOp("chat_apply_fixes")

	type pending struct {
		prop ai.FixProposal
		key  fixContentKey
	}
	pend := make([]pending, len(props))
	appliedFlags := make([]bool, len(props))

	// Associate every target with the CURRENT doc's analysis: exact BlockID
	// first; the props were built from this flow's content moments ago, so a
	// miss means a concurrent edit — the content key still gives a chance.
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

	// findFinding locates the not-yet-consumed finding matching key, preferring
	// an exact BlockID+Rule hit. consumed is by finding index.
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
	// B1.11: analysis is only re-run when the doc CHANGED since the last one
	// (a failed/skipped item leaves the doc — and the report — current), and
	// the last mid-loop analysis doubles as the verification report when
	// nothing applied after it. A 20-item batch with k failures used to run
	// k+1 redundant full 41-rule walks on an unchanged document.
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
		// Mark findings consumed by EARLIER items of this same analysis pass
		// so identical-sibling association stays one-to-one.
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
			// Gone before we touched it: either an earlier fix in this batch
			// cascaded it away or the user's concurrent edit removed it —
			// from the outcome view, the finding no longer exists.
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
		// The applied fix re-parsed the doc: the report (and its block IDs)
		// are stale — force re-analysis for the next association.
		curReport = nil
		appliedFlags[i] = true
		anyApplied = true
		metrics.RecordFlowOp("chat_apply_fix")
		// Placeholder keeps out.Items index-aligned with pend (every
		// iteration appends exactly one); the verification pass fills it.
		out.Items = append(out.Items, batchItemOutcome{RuleID: pend[i].prop.RuleID})
	}

	// Final verification: an applied item whose finding (by content key) still
	// exists on the end-state flow did not actually resolve.
	if anyApplied {
		// B1.11: when the LAST mid-loop analysis already reflects the final
		// doc (no apply after it), reuse it — only re-run when a trailing
		// apply staled it.
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
	// Fill statuses for skipped items ("already-resolved" already set above).
	for i := range out.Items {
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
