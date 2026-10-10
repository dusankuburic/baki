# 26. Tool specs with scopes, a two-point gate, and one approvals component

Date: 2026-10-10. Status: accepted, implemented (Phase 4b, tools wave 1; the frontend `ApprovalCard` ships in the
same wave).

## Context

The chat's tool registry (`internal/ai/tools.go`) held eight tools as `registeredTool{def, label, handler,
timeout}`. Nothing in it said that `apply_fix` and `apply_fixes` mutate the flow, need the `write` token scope or
ask the user first: the approval was implicit in their handlers, the scope was checked only inside the fix applier
at execution (`requireFixWriteScope`), and the service named the two tools in two predicate lists of its own
(`isFixTool`, `isFixToolName`). Every stream offered all eight tools regardless of its actor, so a chat-only API
token's model was offered `apply_fix`, called it, burnt a paid iteration and got a refusal string. The approval
wait (register, emit `fix_proposal`, wait with keepalives, timeout, emit `fix_decision`, metrics) was written out
twice, once per fix tool. A proposal pending at shutdown returned `ctx.Err()` without resolving the card: the
client showed it until the slot was torn down, then the `SERVER_RESTARTING` bubble. No authz-matrix actor held
`chat+write`, so the only token able to decide an approval was untested at the router.

## Decision

1. **`ai.ToolSpec` replaces `registeredTool` in place.** `{Def, Label, Timeout, RequiresApproval, RequiredScope,
   PreviewKind, Exec}`. `Def` stays the wire `ToolDefinition`, so the four provider converters are untouched.
   `RequiredScope` holds an `auth.Scope*` constant (`internal/ai` imports `internal/auth`; cycle-free).
   `PreviewKind = "fix-patch"` (`ai.PreviewKindFixPatch`) for both fix tools. `ai.ToolRequiresApproval(name)`
   replaces the service's name lists; `ai.ToolSpecs()` is pinned line by line in `internal/ai/testdata/tools.golden`
   (`-update-tools`), and `TestToolSpecs_Complete` requires every approval-gated tool to name a scope and a preview
   kind.
2. **A two-point gate.** `ai.ToolGate = func(ToolSpec) bool`, built once per stream by `ai.NewToolGate(actor,
   approvalsWired)` from `auth.ActorFromContext(ctx)` and whether the fix applier is wired. It is applied at the
   offer (`ai.ToolDefinitionsFor(gate)` on the native request, `ai.ToolPromptInstructionsFor(gate)` in the prompt
   loop) AND in `ai.ExecuteTool` through `ToolContext.Allow`, before any handler. The offer check saves the wasted
   iteration; the execution check is the boundary, because a model can name a tool it was not offered (a
   hallucinated call, a replayed history, an Azure agent's own `function_call`, which stays "unknown tool" and never
   runs a handler). A nil actor (local mode) restricts nothing, as the applier's scope check always did;
   `requireFixWriteScope` stays as the layer inside the applier and at apply time. Refusals are counted in
   `pad_chat_tool_refused_total{tool, reason=scope|approval_unavailable}`.
3. **One approvals component on `ChatService`** (`internal/service/chat_approvals.go`): `requestApproval(ctx,
   streamID, approvalRequest, emit, touch)` registers the request, emits the card, waits with the 15 s keepalive
   for `FixDecisionTimeout` or the stream's end, and resolves the card on timeout and cancellation; `ResolveApproval
   (ctx, streamID, id, approved, excluded)` is the decision's entry, which checks that the stream owns the request
   and that a scoped caller holds the tool's `RequiredScope` (the route asks for `write`; this holds for a tool with
   another scope). First decision wins, as before. Both fix paths use it; the apply logic stays in `chat_fixes.go`.
4. **The wire keeps its names.** The SSE events stay `fix_proposal` / `fix_decision`, and `POST
   /api/chat/fix-decision` stays the one decision endpoint with the same body and policy (`Scopes(chat, write)`,
   documented as "approval decision"). `fix_proposal` gains `tool`, `previewKind`, `requiredScope` and ALWAYS
   `items[]`; a single fix keeps its flat fields too, because clients before this release synthesise one item from
   them. `fix_decision` gains the status `cancelled`, emitted from the `ctx.Done()` branch before the stream's
   `SERVER_RESTARTING` (or `STREAM_STOPPED`) error. The frontend `ApprovalCard` renders by `previewKind`
   (`fix-patch`; a generic fallback for unknown kinds; `fix-patch` when absent) and also parses
   `approval_request` / `approval_decision` for a later server. Renaming the events or adding
   `/api/chat/approval-decision` waits until no client older than one release exists and a second preview kind
   ships: either would add golden rows and swagger now for nothing, and a renamed event would make every old client
   miss the card and time out.
5. **Storage stays the conversation message.** `FixProposalSnapshot` gains `tool` and `previewKind` (JSON names
   otherwise unchanged; `cancelled` joins the stored card statuses). An approval lives inside one stream for at
   most 60 s and is only meaningful while the tool call waits; a durable table would need resumable tool loops to
   be of any use. The pending map stays in-process: that is Finding 15's "chat begin/cancel/fix-decision" item and
   Phase 5b's, not this one's.
6. **The authz matrix gains `pat:chat+write`.** Its `/api/chat/fix-decision` row is 403 like `pat:chat`'s, but
   from the ownership check (`STREAM_NOT_FOUND` for the matrix's unknown stream), not the scope gate;
   `TestApprovals_RoundTripThroughRouter` tells the two apart by code and drives a real approval through the router.

## Consequences

- A chat-only token's model is offered six tools; `apply_fix` called anyway answers "not available in this session"
  without a card, and the metric says so. Interactive sessions and local mode see no change in what is offered.
- A card pending when the server stops (or the user stops the response) resolves as `cancelled` with a message
  before the error bubble; clients of the previous release clamp the unknown status to `error`, which reads the
  same. Nothing is written in either case.
- Every approval request carries the same envelope, so the card needs no fix-specific parsing to decide how to
  render, and a new approval-gated tool is a `ToolSpec` plus a `requestApproval` call: no new event, endpoint,
  golden row or frontend card.
- Two service-side name lists are gone; the registry is the one place that says what a tool needs, and the golden
  makes a change to that reviewable.
- `pad_chat_fix_decisions_total` keeps its name (dashboards name it) and gains the `cancelled` status.

## Rollback

No schema change: `tool` and `previewKind` are optional JSON fields inside the messages document, which the previous
release's structs ignore on read and drop on the next write of that conversation (the card still renders: both
default to a fix patch). A client of this release against the previous backend reads events without the new
fields and defaults `previewKind` to `fix-patch`; a client of the previous release against this backend reads the
flat fields of a single proposal and `items[]` of a batch as before, and clamps `cancelled` to `error`. The
authz-matrix, metrics and tools goldens are refreshed in the same change.

## Open

- The pending approvals are process memory: a decision POST that lands on another replica, or on the process
  after a restart, answers 404 (Phase 5b's resumable loops).
- `approval_request` / `approval_decision` and `/api/chat/approval-decision` as aliases, once a second preview
  kind exists and no client older than one release does.
- The prompt loop embeds tool results unescaped inside `<tool_result>` markers; a value containing the closing
  marker ends the block early (a misleading answer at worst: every mutation sits behind the card).
- The journal cap (100 events, oldest half dropped) can lose a proposal from a resume replay in a long tool turn.
