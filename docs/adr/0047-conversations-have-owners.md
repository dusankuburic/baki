# 47. Conversations have owners; keys, settings and billed routes are held to what they promise

Date: 2026-10-28. Status: accepted, implemented (AI roadmap open-item plan, phase 2: security and data integrity). Amends ADR 0031 (conversation keys), ADR 0005 (key rotation), ADR 0012 (section writes) and ADR 0027 (unchanged uploads).

## Context

A stored conversation was keyed only by flow and client-chosen key, so every viewer of a flow could read and extend another user's thread by key, and the predictable keys (`flow`, a block id) were shared by construction. Threads lived only in the browser's `localStorage`, so a new browser lost them and a closed or evicted thread could not be reached again. Account erasure left the user's conversations on other people's flows.

Beside that:
- **Keys:** the keystore silently fell back to the auth secret, so rotating the JWT secret broke every stored credential.
- **Org settings:** concurrent writes lost one of them, including the fail-closed provider policy.
- **Export:** the data export omitted the user's AI usage records.
- **Billed routes:** they were a hand-kept list.
- **Re-uploads:** an identical re-upload paid for every embedding again.
- **Usage rows:** they named the chain's user rather than the one the call was attributed to.
- **Agent tools:** a Foundry agent's hosted tools left no record.

## Decision

1. **Conversations have an owner** (migration 0052, expand: `owner_id`, `title`, `created_at`, `message_count`).
   - Every server-side write stamps the caller, and the store applies the owner rule in its SQL. Chat routes run outside the request's row-level-security transaction; the new policies also hold the rule for RLS paths.
   - Another user's key reads as an empty conversation. A write to it answers 409 `CONVERSATION_KEY_TAKEN`, and the client moves its thread to a new key.
   - Owners are immutable: a trigger refuses a change or removal of a set owner.
   - The primary key stays `(flow_id, scope)` for this release, because the previous release upserts on it. Keying by owner is the pending contract step.
2. **Legacy rows are claimed or kept shared.**
   - **Claimed:** an unowned row under a client thread id (a canonical lower-case uuid that no message names as a block) is claimed by its first owned write: a turn, a client save or a title.
   - **Shared:** rows under `flow` or a block id stay shared for this release. They are readable and appendable by the flow's audience. Replacing, renaming or clearing them needs the right to delete the flow (403 `CONVERSATION_UNOWNED`).
3. **A server-side thread index.**
   - `POST /api/chat/threads` lists the caller's threads, and `POST /api/chat/title` names one. The client merges the server's threads into its index without ever switching the active thread, and syncs titles.
   - Viewers may clear their own threads.
   - In the web app, the thread cap's eviction only drops a thread locally, because the server index keeps it reachable. On the desktop, and in deployments without accounts, it still deletes.
   - Erasure deletes the user's conversations on every flow. Export carries the user's own threads with their titles.
4. **Flow deletion states its authority.** Under row-level security, the storage `DeleteFlow` refuses unless the context carries the caller's `FlowDelete` decision for that flow, because it deletes every user's threads on the flow as the system.
5. **Encryption keys.**
   - The auth-secret fallback for the keystore key is deprecated, with removal on 2027-02-01. The boot warning says exactly what to set. It is not refused yet, because existing deployments seal under it.
   - Infra requires `encryptionKey`, and the deploy workflow refuses a missing or short secret.
   - The strength rule covers local database deployments. Previous keys are exempt, so a weak key can be rotated away.
   - `bakicli keys check` reports, by name, which key opens each sealed value.
6. **Org settings section writes.** `PATCH /api/system/settings/org/{id}/sections/{section}` merges fields under the section's row locks, with an optional `ifRevision` precondition (409 `SETTINGS_STALE`). The providers form uses it and falls back to the whole-document write only on an old replica's 404 or 405.
7. **The user data export** carries `aiUsage`.
8. **Billed routes are derived and bucketed.**
   - `route.Billed()` is required exactly on the handlers whose call graph reaches a provider Chat, Stream or Embed; a test builds the graph.
   - A billed route must use a cost bucket: preview-context moves to `chat`, and knowledge upload and re-index get `embed`.
   - An identical re-upload (same scrubbed text, chunker version and embedding identity) makes no embedding call and reports `unchanged`.
9. **Attribution and audit.**
   - The usage row names the scope and organisation the call was attributed to (`WithUsageScope`).
   - An agent's hosted tools are recorded in a `chat.agent_tools` audit row at the end of the turn.
   - A spec may not combine a Resolver with org sharing.

## Consequences

- A flow viewer can no longer read or extend another user's owned thread. The shared legacy rows remain readable by the flow's audience until the contract step removes them.
- Another user's thread id gives an empty conversation whose turn is not stored. The client re-keys on the next save.
- Rotating `PAD_AUTH_SECRET` remains unsafe on deployments that still rely on the fallback, until they set `PAD_ENCRYPTION_KEY` and re-seal. `bakicli keys check` tells them when it is safe.
- The next release's contract step keys conversations by owner and removes the shared rows.
