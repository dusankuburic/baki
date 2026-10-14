package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
)

// The approvals component of the chat service (ADR 0026): a tool that must ask the stream's owner before acting
// registers an approvalRequest, which shows the card (the fix_proposal event), waits for the decision with
// keepalives, and resolves the card (fix_decision) on every exit — decided, timed out, or cancelled with the
// stream. The decision arrives through DecideApproval from POST /api/chat/fix-decision: ResolveApproval here, or,
// for a stream another replica runs, a command to that replica (chat_commands.go, ADR 0033). The pending set is
// process memory: an approval lives inside one stream for at most decisionWait, and nothing restarts a tool
// loop after a process restart.

// fixDecisionTimeout bounds the approval wait; a var so tests can shorten it (fixDecisionWait).
var fixDecisionTimeout = ai.FixDecisionTimeout

// decisionWait resolves the effective approval-wait duration (test override aware).
func (s *ChatService) decisionWait() time.Duration {
	if s.fixDecisionWait > 0 {
		return s.fixDecisionWait
	}
	return fixDecisionTimeout
}

// fixDecisionKeepalive is how often the approval wait touches the activity clock: without it the idle watchdog
// (counting only provider chunks + explicit touches) could cancel a healthy stream while the user reads.
const fixDecisionKeepalive = 15 * time.Second

// ErrNoPendingFix is returned by ResolveApproval for an unknown, already-decided or other-stream proposal ID.
var ErrNoPendingFix = errors.New("no pending fix proposal for this stream")

// approvalCancelledMessage is the fix_decision message of a card resolved by the stream ending (the server
// stopping, the user stopping the response) before a decision.
const approvalCancelledMessage = "the response ended before a decision was made — nothing was changed"

// approvalRequest is one approval a tool asks of the stream's owner: the envelope the card renders by (tool,
// previewKind, requiredScope, items[]) plus the kind-specific flat fields of the event. Every request carries
// items[], one per thing approved; a single fix repeats its item as flat fields for clients that read only those.
type approvalRequest struct {
	id            string
	tool          string
	previewKind   string
	requiredScope string
	items         []map[string]interface{}
	fields        map[string]interface{}
}

// approvalRequestFor builds a request for spec's tool, so the envelope cannot drift from the registry.
func approvalRequestFor(spec ai.ToolSpec, id string, items []map[string]interface{}, fields map[string]interface{}) approvalRequest {
	return approvalRequest{id: id, tool: spec.Def.Name, previewKind: spec.PreviewKind, requiredScope: spec.RequiredScope, items: items, fields: fields}
}

// eventData is the fix_proposal payload.
func (r approvalRequest) eventData() map[string]interface{} {
	data := map[string]interface{}{
		"proposalId":    r.id,
		"tool":          r.tool,
		"previewKind":   r.previewKind,
		"requiredScope": r.requiredScope,
		"items":         r.items,
	}
	for k, v := range r.fields {
		data[k] = v
	}
	return data
}

// approvalDecision is the owner's verdict: approved, with the item indices they deselected.
type approvalDecision struct {
	approved bool
	excluded []int
}

// approvalOutcome says how a request ended.
type approvalOutcome uint8

const (
	approvalDecided approvalOutcome = iota + 1
	approvalTimedOut
	// approvalCancelled: the stream ended first; the card was resolved with status cancelled.
	approvalCancelled
)

// fixDecisionState is one registered approval prompt; decided is buffered so ResolveApproval never blocks and
// the FIRST decision wins.
type fixDecisionState struct {
	streamID string
	// requiredScope is the tool's scope, checked again on the decision (ResolveApproval).
	requiredScope string
	decided       chan fixDecision
}

// fixDecision is one user verdict, delivered whole on the channel.
// excludedBatchIdx carries per-item OPT-OUTS for a batch approval (U4.1): indices into the card's item list the
// user deselected. It lives in the message, not on fixDecisionState: a dropped send (a double-clicked Approve or
// retry) still wrote the shared field, racing the consumer on an unsynchronised slice header (proven under
// -race); carrying the payload means only the winning decision is ever published.
type fixDecision struct {
	approved         bool
	excludedBatchIdx []int
}

// requestApproval shows the card and waits for the decision, touching the activity clock every
// fixDecisionKeepalive, for decisionWait or until the stream ends. Timeout and cancellation resolve the card
// here; a decided request is resolved by the caller once it knows the outcome of acting on it.
func (s *ChatService) requestApproval(ctx context.Context, streamID string, req approvalRequest, emit func(string, map[string]interface{}), touch func()) (approvalDecision, approvalOutcome) {
	st := &fixDecisionState{streamID: streamID, requiredScope: req.requiredScope, decided: make(chan fixDecision, 1)}
	s.pendingFixDecisions.Store(req.id, st)
	defer s.pendingFixDecisions.Delete(req.id)
	// Other replicas learn of the approval (stream, owner replica, required scope) before the card is shown, so a
	// decision that lands on one of them reaches this stream (DecideApproval). Best effort.
	s.cluster.registerApproval(req.id, streamID, req.requiredScope, s.decisionWait())
	defer s.cluster.dropApproval(req.id)

	emit("fix_proposal", req.eventData())

	timeout := time.NewTimer(s.decisionWait())
	defer timeout.Stop()
	keepalive := time.NewTicker(fixDecisionKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case dec := <-st.decided:
			return approvalDecision{approved: dec.approved, excluded: dec.excludedBatchIdx}, approvalDecided
		case <-keepalive.C:
			if touch != nil {
				touch()
			}
		case <-timeout.C:
			emitApprovalDecision(emit, req.id, "timeout", "")
			return approvalDecision{}, approvalTimedOut
		case <-ctx.Done():
			// Resolved before the stream's terminal event: the client would otherwise show the card until its
			// slot is torn down, and the record of the turn would never say what became of it.
			emitApprovalDecision(emit, req.id, "cancelled", approvalCancelledMessage)
			return approvalDecision{}, approvalCancelled
		}
	}
}

// emitApprovalDecision resolves a card: the fix_decision event, counted by status.
func emitApprovalDecision(emit func(string, map[string]interface{}), id, status, message string) {
	recordChatFixDecision(status)
	data := map[string]interface{}{"proposalId": id, "status": status}
	if message != "" {
		data["message"] = message
	}
	emit("fix_decision", data)
}

// ResolveApproval delivers the caller's decision on a pending approval: the stream must own it, a scoped token
// must hold the scope the tool requires (the route asks for write; this holds for a tool with another scope),
// and the first decision wins. A nil actor (local mode) is not restricted.
func (s *ChatService) ResolveApproval(ctx context.Context, streamID, proposalID string, approved bool, excludedBatchIdx []int) error {
	v, ok := s.pendingFixDecisions.Load(proposalID)
	if !ok {
		return ErrNoPendingFix
	}
	st := v.(*fixDecisionState)
	if st.streamID != streamID {
		return ErrNoPendingFix
	}
	if actor := auth.ActorFromContext(ctx); st.requiredScope != "" && actor != nil && !actor.Allows(st.requiredScope) {
		return fmt.Errorf("%w (deciding this approval needs %s)", ErrScopeDenied, st.requiredScope)
	}
	dec := fixDecision{approved: approved}
	if approved {
		dec.excludedBatchIdx = excludedBatchIdx
	}
	select {
	case st.decided <- dec:
	default: // already decided — first decision wins
	}
	return nil
}

// ResolveFixDecision is ResolveApproval without a caller: the decision of the stream's owner, already
// authorized by the route.
func (s *ChatService) ResolveFixDecision(streamID, proposalID string, approved bool, excludedBatchIdx []int) error {
	return s.ResolveApproval(context.Background(), streamID, proposalID, approved, excludedBatchIdx)
}
