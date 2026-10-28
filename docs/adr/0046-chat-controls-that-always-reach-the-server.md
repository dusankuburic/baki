# 46. Stream controls always reach the server, the watchdog says why it stopped, and tools are on by default

Date: 2026-10-27. Status: accepted, implemented (AI roadmap open-item plan, phase 1: chat correctness). Amends ADR 0043 (the watchdog's wire codes) and ADR 0031 (the version refresh after an unadoptable done).

## Context

Several defects made the chat wrong in ordinary use:

- **Stop could be silently refused.** A Stop or an approval decision shared the general per-address rate-limit bucket and the per-user write cap with every other request. When either refused it, the client dropped the Stop while the billed stream ran on.
- **Watchdog stops looked like the user's.** The watchdog's cancellations used the user's own `STREAM_STOPPED` code. A setup stall was blamed on the provider.
- **Long turns lost decisions.** On a long agentic turn, the journal's bound could evict an approval and its decision.
- **Tools were off by default.** The assistant answered flow questions from a context summary it could not check.
- **Lost or mislabelled follow-ups.** A second queued follow-up silently replaced the first, and the context chip's templates named the canvas selection, not the thread's block.
- **Screen-reader noise.** The usage meter re-announced a token count several times a second, and scrolling the virtualised history re-announced old messages.
- **Resume and version gaps.** A raced resume dropped the gap's tool and approval events. A server-owned turn the client could not adopt left a stale version, so the next Clear or Compact falsely reported "changed elsewhere".

## Decision

1. **Stream controls have their own bucket.**
   - **The bucket:** `POST /api/chat/cancel` and `/api/chat/fix-decision` are limited by a per-address `control` bucket (`PAD_RATE_LIMIT_CONTROL_RPS`/`_BURST`, default 2 rps, burst 20), checked before authentication, and are exempt from the per-user write cap.
   - **Why not no limit:** an unauthenticated flood of token checks would have no bound at all.
   - **Why not a per-user control ceiling:** sharing the per-user limiter's rate would tie Stop to the write cap an operator tightens. The cost of a control request is at most an ownership lookup.
   - **The client:** it retries a 429 per `Retry-After`. It keeps a stopping stream's subscription until the server confirms the stop or the stream ends, and it says the server was busy rather than unreachable.
2. **The watchdog has its own codes, and names the phase.**
   - **Codes:** `STREAM_IDLE_TIMEOUT` and `STREAM_CLIENT_GONE`. The turn record maps them to the step-8 outcome labels `idle_timeout` and `subscriber_gone`.
   - **Phase:** the stream records its phase (setup, provider, tool, approval, apply) and the idle message names it.
   - **Idle clock:** it is touched at the setup boundaries, so a setup stall is reported as one.
   - **Stored record:** a watchdog-stopped turn is stored as an error, as the client shows it.
3. **Approvals are never evicted from the journal.** Tool calls are dropped as whole calls, each start paired with its result; a start that never gets a result does not hold the cut open. One `journal_truncated` marker says how many earlier calls were omitted.
4. **Tools are on by default.**
   - `useTools` defaults to on, with an AI setting to turn the default off (`toolsOffByDefault`). A thread's own choice persists, including off.
   - Lookup templates and suggested prompts turn tools on for the thread when the send goes ahead, and the composer says so.
5. **Follow-ups queue in order.** Up to five per thread. The queue holds after an error or the user's Stop instead of sending the next one into the same failure.
6. **One live announcement per answer.**
   - The message list stays `role="log"` but with `aria-live="off"`: a log is a live region by default, so dropping the attribute alone would still announce every scroll.
   - Each committed answer is announced once, in its own sr-only region.
   - The token count is `aria-hidden`.
7. **A resume applies its events before its end.** One `applyResume` serves the delta resume, its race refetch and the stall probe.
8. **The version follows the server's own turn.** A done the client cannot adopt triggers one refresh of the thread's version. The refresh is refused when another writer added or removed messages, so the truthful "changed elsewhere" stays.

## Consequences

- A user's Stop and decisions stop the billing whatever else the client is sending.
- An operator can tell a watchdog stop from a user's, and a setup stall from a silent provider.
- Threads that never chose a tools setting now run the grounding tool loop. On providers without native tools that is the marker loop, whose instructions cost context on small windows.
- Queued follow-ups are not persisted across a reload, and they send with the thread's context at the time they are sent.
- There is no per-user ceiling on stream controls across many addresses. It is acceptable for a single-container internal tool and is recorded in `operations.md`.
