# 24. Graceful shutdown: drain first, and end chat streams with SERVER_RESTARTING before SSE closes

Date: 2026-10-10. Status: accepted, implemented (Phase 5a).

## Context

The stop hook closed SSE connections before cancelling chat streams, so no client heard why its answer stopped: without Redis the client's stall probe reported "stalled" 90-120 s after every deploy. Chat workers were bare goroutines nothing waited for, and the server accepted new streams and sockets until `server.Shutdown`, with no draining state.

## Decision

1. **A draining state** (`internal/platform/drain`, `drain.State` one-way, `drain.Process()` for the process). At once: `/readyz` and `/api/health` answer 503 `{"status":"draining"}` from the first probe; `/healthz` stays 200. `POST /api/chat/stream` is refused before the body is read; `GET /api/events` refuses new streams; `GET /ws` refuses before the ticket is redeemed (the ticket stays unspent).
2. **The refusal** `drain.Refuse`: 503, `Retry-After: 2`, `Connection: close`, envelope `{"code":"SERVER_RESTARTING", …, "details":{"retryAfterSeconds":2}}`; not logged, message not masked (the one exception to the 5xx rules of api-conventions.md).
3. **`ChatService.Shutdown(ctx)`** runs while SSE is still open: sets `closing` (new streams refused `ErrServerRestarting`), cancels every active stream with `ChatErrServerRestarting` (code `SERVER_RESTARTING`; the first reason and its code are kept — a user-stopped stream stays `STREAM_STOPPED`), waits ≤2 s for the worker WaitGroup (each worker's `Done` runs after the terminal emit and the answer persist), then force-ends streams that recorded no terminal state (`SERVER_RESTARTING` recorded and emitted; the first terminal state wins, the late answer is not stored). Logs: INFO `chat streams ended for shutdown`, WARN `chat workers still running at shutdown`.
4. **SSE writes what is queued when it closes** (one 1 s write deadline), so queued terminal events are not dropped at random.
5. **A resume of an unknown stream says so:** `/api/chat/resume`/`begin`/`cancel`/`fix-decision` answer code `STREAM_NOT_FOUND` (403 with accounts — unknown and another user's not told apart; 404 without); clients treat 403/404/410 from a reconnect resume as "gone".
6. **Stop order and budget.** `registerDrain` is the last Invoke so its hook runs first: the drain pause (`PAD_SHUTDOWN_DRAIN_DELAY`, cloud default 3 s, max 5 s — a longer value refuses to boot) then `ChatService.Shutdown`. `fx.StopTimeout` is 28 s under a 35 s grace. Hooks in order: drain (3 s + 2 s), backfills cancelled, audit backfill (5 s), scanner (5 s), server (router/WebSocket 3 s, then `server.Shutdown` + `DrainAll` on 8 s), retention purge (5 s), telemetry flush (5 s), auth manager (5 s), audit flush (5 s), pool close (15 s). As designed a healthy stop spends 3+2+5+3+8 ≈ 21 s plus the audit flush ≈ 26 s. This table is the one place the budget is written.
7. **Client half:** keys on the terminal event's `code` (same code honoured in a resume/begin `errorCode`), the refused create's envelope code with `details.retryAfterSeconds` (capped 10 s + jitter), and 403/404/410 meaning gone. On `SERVER_RESTARTING` the turn is re-sent once automatically (partial text kept, one flag per turn so no loop); excluded: fix-proposal turns, re-attached streams, unsynced threads.

## Consequences

A deploy with an open chat hears `SERVER_RESTARTING` before the connection drops and re-sends the turn; an answer that finished just before the stop is stored before the pool closes (within the 2 s budget). A stopping replica leaves rotation on its first readiness probe. A cancelled stream stores nothing (its question was stored at start) and the resend asks it again. Rollback: no schema, config or stored-format change; the previous release ignores all of this, and a previous-release replica relays a snapshot's `SERVER_RESTARTING` as any other code.
