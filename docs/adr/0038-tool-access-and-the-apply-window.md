# 38. Tool access follows flow permission, and an approved apply has its own window

Date: 2026-10-24. Status: accepted, implemented (AI roadmap step 3). Amends ADR 0026: the gate's inputs and its result, and the approval tools' deadline.

## Context

ADR 0026's gate decided what a stream is offered from the actor's token scopes and whether an approval applier is wired. A viewer of a flow, who may chat about it but not edit it, was still offered `apply_fix` and `apply_fixes`; the call was refused only at apply time, after the user had seen and approved a card. The system prompt always said `apply_fix` was the way to change the flow, also on streams with no tools at all.

`ExecuteTool` wrapped every tool in one deadline, and an approval tool's was 120 s over both the user's decision and the apply with its verifying re-analysis. A user who took 55 s to approve left 65 s for the write, the re-parse and the analysis; a batch of ten fixes shared that window, and a verification cut by the deadline reported a working fix as unresolved.

A panic in a tool was turned into an error-shaped result only on the parallel read-only path. On the other paths it reached the stream worker's recover, the stream ended as interrupted, and an approval card that had been approved stayed open.

## Decision

1. **The gate has three inputs and returns a reason.** `ai.NewToolGate(actor, approvalsWired, canEditFlow)` returns an `ai.ToolGate = func(ToolSpec) string`: empty when the tool is allowed, else `approval_unavailable`, `scope` or `flow_permission`, in that order. `ChatService.canEditFlow` checks `FlowEdit` once when the stream builds its gate; the apply-time `FlowEdit` check and `requireFixWriteScope` stay. The refusal metric's `reason` label takes the gate's reason.
2. **The system prompt follows tool access.** `ai.ToolAccessOf(useTools, gate)` gives none, read-only or fixes, and `ai.SystemPromptFor(access)` one of three byte-stable prompts: with no tools the model is told it cannot change anything and should explain the change; with read-only tools it may preview with `propose_fix`; only the fixes prompt names `apply_fix`. `SystemPromptDefault` is the fixes prompt. The access is part of the `chat.context` cache key. The marker-mode instructions name `apply_fix` only when the gate offers it.
3. **An approval tool has no tool deadline.** `ExecuteTool` runs a tool whose spec `RequiresApproval` on the stream's context; its `Timeout` is ignored and left zero. The applier bounds its own phases: the decision waits `FixDecisionTimeout` (60 s), then the apply and its verification run under `fixApplyTimeout(n)` = 30 s + 30 s × fixes, started at the approval. A verification that runs out of that window reports the fix as applied and not verified; an apply that runs out of it returns a model-readable error and resolves the card as `error`; batch items the window never reached are reported as not attempted.
4. **Every tool path is panic-guarded.** `guardedToolRun` wraps every tool execution in both loops. Both appliers defer a recovery that resolves a still-open card as `error` and returns an error-shaped result saying the flow may already have changed.
5. **The prompt-facing text is pinned.** `tools.golden` carries every tool's description and canonical input schema (`-update-tools`); `prompts.golden` the three system prompts and the marker-mode instructions (`-update-prompts`).

## Consequences

A viewer's model is offered six tools and told it cannot apply a fix; no card is shown that apply time would refuse. The slow-approval and large-batch failures are gone, and a ten-fix batch gets five and a half minutes, well inside the stream's wall clock; the idle watchdog is still touched every 15 s through the window. A stream whose flow permission changes mid-turn keeps the gate it started with, and the apply-time check still refuses the write. A change to a tool description, a schema or a system prompt now shows up as a golden diff for review. Two ten-fix batches in one turn can outlast the stream's ten-minute clock. Rollback: no schema or wire change; the `flow_permission` refusal label disappears from the metric.
