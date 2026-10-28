package service

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/audit"
	"pad-core/logger"
)

// Outcome labels of the cancellations: the user's stop (STREAM_STOPPED), and the watchdog's idle cancel
// (STREAM_IDLE_TIMEOUT) and disconnect cancel (STREAM_CLIENT_GONE).
const (
	turnOutcomeStopped        = "stopped"
	turnOutcomeIdle           = "idle_timeout"
	turnOutcomeSubscriberGone = "subscriber_gone"
)

// chatFailureCodes are the failure codes a turn can end with; pad_chat_turns_total pre-initialises a label for
// each (lowercased), so a rate over a code that has not happened yet reads 0, not "no data".
var chatFailureCodes = []string{
	ChatErrProviderUnavailable, ChatErrProviderRateLimited, ChatErrProviderAuth, ChatErrProviderBalance,
	ChatErrProviderNotConfigured, ChatErrContextLimit, ChatErrContextUnavailable, ChatErrStopped, ChatErrTimeout,
	ChatErrInterrupted, ChatErrToolLoopLimit, ChatErrHistoryUnavailable, ChatErrContentFiltered,
	ChatErrProviderPermission, ChatErrOutputLimit, ChatErrModelNotAvailable, ChatErrServerRestarting,
	ChatErrProviderUnknown, ChatErrProviderNotAllowed, ChatErrIdleTimeout, ChatErrClientGone,
}

// chatTurnOutcomes lists every outcome label, once each.
func chatTurnOutcomes() []string {
	out := []string{"done", "error"}
	for _, code := range chatFailureCodes {
		if label := turnOutcome(false, code); !slices.Contains(out, label) {
			out = append(out, label)
		}
	}
	return out
}

// turnOutcome is the label of how a turn ended, as its client saw it: done, the lowercased failure code, or for a
// cancellation its own label (stopped, idle_timeout, subscriber_gone); error when unclassified.
func turnOutcome(done bool, code string) string {
	switch code {
	case "":
		if done {
			return "done"
		}
		return "error"
	case ChatErrStopped:
		return turnOutcomeStopped
	case ChatErrIdleTimeout:
		return turnOutcomeIdle
	case ChatErrClientGone:
		return turnOutcomeSubscriberGone
	}
	return strings.ToLower(code)
}

// turnRecord is what a turn's terminal record says beyond the stream's own state. start, provider, orgID and
// logCtx are set before the stream is published; model and the counts are under the stream's mu.
type turnRecord struct {
	start    time.Time
	provider string
	// orgID is the flow's organisation ("" without one): the chat.agent_tools audit row's.
	orgID string
	// logCtx is the worker's context: its request (request_id, user_id), its span and its stream_id.
	logCtx context.Context
	// recorded is claimed by the one call that writes the record: the worker's cleanup, or Shutdown in a stuck
	// worker's place.
	recorded   atomic.Bool
	iterations atomic.Int32
	model      string
	toolCalls  int
	approvals  map[string]int
	// hostedTools counts the provider's own tools that ran, by output item type (ai.ToolEvent.Name: web_search_call,
	// mcp_call, …): an agent's hosted tools, which baki's tool approvals never see (R-provider-29).
	hostedTools map[string]int
}

// maxHostedToolTypes bounds the distinct hosted tool types one turn records: the names are the provider's item
// types, a small closed set, so the cap only guards the audit row against a misbehaving upstream.
const maxHostedToolTypes = 16

// hostedToolEvent accounts one provider ToolEvent: the hosted-step idle limit (hostedStep) and the turn's record
// of the hosted tool types that ran — a start counts a call; a completion whose start was never seen still names
// its type.
func (c *streamCtl) hostedToolEvent(ev *ai.ToolEvent) {
	c.hostedStep(!ev.Done)
	name := ev.Name
	if name == "" {
		name = "unknown"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r := &c.turn
	if r.hostedTools == nil {
		r.hostedTools = map[string]int{}
	}
	if _, seen := r.hostedTools[name]; !seen && len(r.hostedTools) >= maxHostedToolTypes {
		return
	}
	switch {
	case !ev.Done:
		r.hostedTools[name]++
	case r.hostedTools[name] == 0:
		r.hostedTools[name] = 1
	}
}

// count tallies a journaled event: executed tools and resolved approval cards (by status). Under the stream's mu.
func (r *turnRecord) count(eventType string, data map[string]interface{}) {
	switch eventType {
	case "tool_result":
		r.toolCalls++
	case "fix_decision":
		status, _ := data["status"].(string)
		if status == "" || status == "applying" {
			return // "applying" is the card's interim state; its final status follows
		}
		if r.approvals == nil {
			r.approvals = map[string]int{}
		}
		r.approvals[status]++
	}
}

// setTurnModel records the turn's requested model once it is resolved.
func (c *streamCtl) setTurnModel(model string) {
	c.mu.Lock()
	c.turn.model = model
	c.mu.Unlock()
}

// recordTurnEnded writes the turn's terminal record once: the "chat turn ended" line, pad_chat_turns_total and
// pad_chat_turn_duration_seconds (R-observability-1). The worker's cleanup calls it, and Shutdown for a stream it
// ended in a stuck worker's place; whichever comes first writes it.
func (s *ChatService) recordTurnEnded(ctl *streamCtl) {
	if !ctl.turn.recorded.CompareAndSwap(false, true) {
		return
	}
	ctl.mu.Lock()
	done, code := ctl.done, ctl.errCode
	if ctl.errMsg == "" {
		code = ""
	}
	tokensIn, tokensOut, served := ctl.tokensIn, ctl.tokensOut, ctl.servedModel
	model, toolCalls, approvals := ctl.turn.model, ctl.turn.toolCalls, maps.Clone(ctl.turn.approvals)
	hostedTools := maps.Clone(ctl.turn.hostedTools)
	ctl.mu.Unlock()
	outcome := turnOutcome(done && code == "", code)
	took := time.Since(ctl.turn.start)
	chatTurnsTotal.WithLabelValues(outcome).Inc()
	chatTurnDuration.Observe(took.Seconds())
	ctx := ctl.turn.logCtx
	if ctx == nil {
		ctx = context.Background()
	}
	logger.FromContext(ctx).Info("chat turn ended",
		"flow_id", ctl.flowID, "conversation", conversationLogID(ctl.flowID, ctl.convKey),
		"provider", ctl.turn.provider, "model", model, "served_model", served,
		"outcome", outcome, "code", code,
		"tokens_in", tokensIn, "tokens_out", tokensOut,
		"iterations", ctl.turn.iterations.Load(), "tool_calls", toolCalls, "approvals", approvals,
		"hosted_tools", hostedTools, "duration_ms", took.Milliseconds())
	s.auditHostedTools(ctx, ctl, model, hostedTools)
}

// auditHostedTools records the chat.agent_tools row of a turn in which the provider ran tools of its own (an Azure
// AI Foundry agent's web search, file search, code interpreter, MCP …): those run outside baki's function-tool
// approvals and can carry the flow context the agent was sent to a third party, so the audit trail names which
// kinds ran (R-provider-29). The chat.stream row is written on accept, before any tool runs; this one when the
// turn ends, tied to it by streamId. No row when none ran.
func (s *ChatService) auditHostedTools(ctx context.Context, ctl *streamCtl, model string, hostedTools map[string]int) {
	if s.audit == nil || len(hostedTools) == 0 {
		return
	}
	calls := 0
	for _, n := range hostedTools {
		calls += n
	}
	tools := slices.Sorted(maps.Keys(hostedTools))
	s.audit.Record(ctx, audit.Entry{
		Action: audit.ActionChatAgentTools, ResourceType: "flow", ResourceID: ctl.flowID, OrgID: ctl.turn.orgID,
		Meta: map[string]string{
			"streamId": ctl.streamID, "provider": ctl.turn.provider, "model": model,
			"tools": strings.Join(tools, ","), "calls": strconv.Itoa(calls),
		},
	})
}
