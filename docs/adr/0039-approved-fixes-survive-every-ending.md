# 39. An approved fix is found again, applied whole, reported in curated words, and recorded

Date: 2026-10-26. Status: accepted, implemented (AI roadmap step 4). Amends ADR 0024 (the decision route while draining, and a stop that waits for an approved apply) and ADR 0026 (the approval envelope's statuses and its shared fixture).

## Context

An approved fix changes the user's flow, and several endings lost or misreported that change:

- An edit during the approval wait re-parsed the flow, so the proposal's block id no longer existed and the apply failed. The batch path matched targets by a name its siblings shared.
- Any apply error, a version conflict included, reached the card and the model as raw storage text.
- A turn that failed after a fix left no record: the server stored only finished turns, and the client's error message dropped the cards.
- A Stop while a fix was being applied stored the card as "Applying…" for good.
- During the drain an approval could still be asked and approved, and the stop then cancelled the apply mid-write.
- A desktop fix's undo lived only in memory.
- Nothing audited the write or the decision.
- The backend's event payloads and the frontend parser shared no fixture.

## Decision

1. **A proposal names its block by content.** `FixProposal.Anchor` is the block's anchor on the document the proposal was made on. An approved fix finds its finding on the flow as it is at the approval (`fixTarget.locate`) in this order:
   1. By the proposal's block id, while the flow was not re-parsed.
   2. Else by the block at the anchor's place. A block still standing without the finding means the finding is gone, never a sibling's.
   3. Else, when the anchor no longer resolves, by the content key.

   A finding gone before the approval resolves the card as `already-resolved` and writes nothing.
2. **A fix that was not applied is an outcome, not an error.** A version conflict is retried once on a fresh read, per item in a batch. Every other failure becomes a curated `error` outcome (`fixFailureDetail`) and the raw error is logged. An applier returns an error only when the stream ended, and the card then says the response ended or the server restarted.
3. **An approved apply is a critical section.** It holds the stream's cancel (`streamCtl.holdCancel`): a Stop or the server's stop waits for it, and `Shutdown` cuts it once its budget is spent. A batch checks before each fix and reports the rest as not attempted, with the reason. The durable undo snapshot of a committed write is pushed under a detached, bounded context.
4. **No approval while draining.** No card is shown, and the model reads that the server is restarting. An approve is refused with 503 `SERVER_RESTARTING` by the replica running the stream; the owner of a relayed decision answers `restarting`. A decline is accepted. The client puts a refused card back to pending, with a retry.
5. **A failed turn that left something behind is stored.** Something behind means fix cards, a tool trail or partial text. The turn is stored as the client shows it: finish reason `error` with `errorCode`, `errorShared` and the error line, or `interrupted` for a stopped turn. A turn the server's stop ended is stored only for its fix cards, because the client re-sends it. The model reads no error line (`historyContent` cuts at the last separator). The client saves keep such a message, and the server's end-of-turn write settles a stored card still pending or applying from the journal.
6. **The desktop undo ring is on disk.** It lives under `<config>/snapshots/<sha256 of the path>/<id>.json` and keeps the cloud ring's bounds: 20 entries, 32 MiB per flow, 24 h. The directory is capped at 256 MiB and swept oldest first at boot. `PatchFlow` stores the snapshot before the write. A restore checks the recorded target and member names against the flow.
7. **Audit.** The chat applier records `chat.fix_decision` and `flow.fix_apply` under the stream's owner and the flow's organisation. The HTTP fix routes record `flow.fix_apply`. A fix that wrote nothing records no write row.
8. **The backend's events are the client's fixture.** `TestChatEvents_Golden` writes every event variant and the resume body to `internal/service/testdata/chat_events/` (`-update-chat-events`). `frontend/src/lib/chatEvent.contract.test.ts` requires every field to be at its place after parsing.

New statuses: the card's `already-resolved`, and the batch item's `skipped` for a fix the user deselected.

## Consequences

- An edit during the wait, a conflict, a Stop, a closed thread, a drain and a desktop restart no longer lose or misreport an applied fix.
- A Stop can take up to the apply window to land while a fix is being written.
- A thread's stored history can now hold failed turns; `historyContent` keeps their error lines out of every prompt.
- `ChatMessage` gained two fields, which cloud storage now keeps along with the omitted-history count and each item's id.
- The snapshot directory grows up to its cap on a desktop.
- A rename on either side of the chat event envelope now fails a test.
- Rollback: no schema change. An older client clamps `already-resolved` and `skipped` to `error`, and older stored messages simply lack the new fields.
