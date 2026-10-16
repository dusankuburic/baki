# 26. Tool specs with scopes, a two-point gate, and one approvals component

Date: 2026-10-10. Status: accepted, implemented (Phase 4b, tools wave 1).

## Context

The chat tool registry said nothing about which tools mutate the flow, need the `write` scope, or ask the user first: approval was implicit in the two fix handlers, the scope was checked only at execution, and every stream was offered all eight tools — so a chat-only token's model was offered `apply_fix`, called it, burnt a paid iteration and got a refusal string. The approval wait was written out twice, and a proposal pending at shutdown left the card unresolved.

## Decision

1. `ai.ToolSpec` replaces `registeredTool` in place: `{Def, Label, Timeout, RequiresApproval, RequiredScope, PreviewKind, Exec}`. `Def` stays the wire `ToolDefinition`; `RequiredScope` holds an `auth.Scope*` constant; `PreviewKind = "fix-patch"` for both fix tools. `ai.ToolRequiresApproval(name)` replaces the service's name lists; `ai.ToolSpecs()` is pinned in `internal/ai/testdata/tools.golden`, and `TestToolSpecs_Complete` requires every approval-gated tool to name a scope and a preview kind.
2. **A two-point gate.** `ai.NewToolGate(actor, approvalsWired)` (`ai.ToolGate = func(ToolSpec) bool`) is applied at the offer (`ToolDefinitionsFor`, `ToolPromptInstructionsFor`) AND in `ai.ExecuteTool` via `ToolContext.Allow` before any handler — the execution check is the boundary because a model can name a tool it was not offered. A nil actor (local mode) restricts nothing; `requireFixWriteScope` stays as the layer at apply time. Refusals counted in `pad_chat_tool_refused_total{tool, reason=scope|approval_unavailable}`.
3. One approvals component on `ChatService` (`internal/service/chat_approvals.go`): `requestApproval(ctx, streamID, approvalRequest, emit, touch)` registers the request, emits the card, waits with 15 s keepalives for `FixDecisionTimeout` or the stream's end, resolves the card on timeout/cancellation; `ResolveApproval(ctx, streamID, id, approved, excluded)` checks the stream owns the request and that a scoped caller holds the tool's `RequiredScope`. First decision wins.
4. The wire keeps its names: SSE events stay `fix_proposal`/`fix_decision`, the endpoint stays `POST /api/chat/fix-decision` (`Scopes(chat, write)`). `fix_proposal` gains `tool`, `previewKind`, `requiredScope` and ALWAYS `items[]` (single fixes keep the flat fields for old clients); `fix_decision` gains status `cancelled`, emitted from the `ctx.Done()` branch before the stream's `SERVER_RESTARTING`/`STREAM_STOPPED` error. The frontend renders by `previewKind` (default `fix-patch`) and parses `approval_request`/`approval_decision` for a later server. Renaming the events or a new `/api/chat/approval-decision` waits until no client older than one release exists and a second preview kind ships.
5. Storage stays the conversation message (`FixProposalSnapshot` gains `tool`, `previewKind`; `cancelled` joins the stored statuses); approvals live in-process for at most 60 s.
6. The authz matrix gains `pat:chat+write`; `TestApprovals_RoundTripThroughRouter` drives a real approval through the router.

## Consequences

A chat-only token's model is offered six tools; `apply_fix` called anyway answers "not available in this session" without a card. A card pending when the server or the response stops resolves as `cancelled` (old clients clamp to `error`); nothing is written. A new approval-gated tool is a `ToolSpec` plus a `requestApproval` call — no new event, endpoint or card. Rollback: no schema; `tool`/`previewKind` are optional fields the previous release ignores and drops on the next write. Open: pending approvals are process memory — a decision on another replica or after a restart answers 404 (Phase 5b); the prompt loop embeds tool results unescaped in `<tool_result>` markers.
