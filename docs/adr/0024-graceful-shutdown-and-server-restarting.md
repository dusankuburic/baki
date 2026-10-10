# 24. Graceful shutdown: drain first, and end chat streams with SERVER_RESTARTING before SSE closes

Date: 2026-10-10. Status: accepted, implemented (Phase 5a). Wave 1 built the backend pieces; wave 2 (F2) the stop
hook that runs them in this order (`internal/app`), the budget below and the client half.

## Context

`fx.Run()` turns SIGTERM into the OnStop hooks (`fx.StopTimeout` was 25 s under ACA's 30 s grace; now 28 s under 35 s,
below). The server's hook ran:

1. `router.Shutdown()`: closes `shutdownCh`, and every SSE handler returns at once;
2. `chatSvc.CancelAll()`: cancels every chat stream ("the server is shutting down", code `STREAM_STOPPED`);
3. the rate limiters, `ShutdownWebSocket` (3 s), `server.Shutdown` and `lifecycle.DrainAll` (8 s).

Three problems followed.

- **R1, the terminal event is lost.** The SSE connections were closed before the streams were cancelled, so no
  client heard why its answer stopped. Without Redis (the ACA default) the new instance answers `/api/chat/resume`
  for the unknown stream with 403 (accounts) or 404, the client dropped that silently, and its stall probe reported
  "stalled" about 90-120 s after every deploy. With Redis, the client got a generic `STREAM_STOPPED` with a manual
  Retry.
- **R2, chat workers were bare goroutines.** Nothing waited for them. A just-finished answer's persist and the final
  resume snapshot raced the pool and Redis closing behind them.
- **No draining state.** `/readyz` answered 503 only after three consecutive failed probes, and the server accepted
  new chat streams, event streams and sockets until `server.Shutdown`.

## Decision

1. **A draining state, `internal/platform/drain`.** `drain.State` is a one-way flag: `Begin()` (true for the call
   that began it; logs `server draining`), `Draining()`. `drain.Process()` is the process's own. A nil `*State`
   stands for it, so a handler built without one follows the server's stop. Tests use `drain.New()`, so beginning
   one does not drain the test binary. Handlers hold a `draining *drain.State` field (`ChatHandler`, `SystemHandler`,
   `EventManager`) or, in `internal/websocket`, a package variable, all nil in production. No fx wiring is needed.
2. **What draining changes, at once.**
   - `GET /readyz` and `GET /api/health`: 503 `{"status":"draining"}` from the first probe, bypassing the
     three-failure smoothing. `/healthz` stays 200: a draining process is not dead.
   - `POST /api/chat/stream`: refused before the body is read. Nothing starts and nothing is audited.
   - `GET /api/events`: a new event stream is refused; the open ones stay open (step 4 below needs them).
   - `GET /ws`: refused before the single-use ticket is redeemed (the router checks before `ConsumeWSTicket`; the
     upgrade handler checks again), so the reconnect may present the same ticket to another replica.
   - Every other request is served until `server.Shutdown`.
3. **The refusal, `drain.Refuse`.** 503, `Retry-After: 2`, `Connection: close` (a retry opens a connection a load
   balancer may route to another replica; Go's HTTP/2 server turns it into a GOAWAY), and the standard envelope
   `{"code":"SERVER_RESTARTING","message":"the server is restarting — try again in a moment","requestId":…,
   "details":{"retryAfterSeconds":2}}`. The web client cannot read response headers (`ApiError` keeps none), so the
   wait is in `details` too. The message is fixed copy and is not masked, and the refusal is not logged: it is the
   one exception to the 5xx rules of [api-conventions.md](../architecture/api-conventions.md#errors).
4. **`ChatService.Shutdown(ctx)`, the chat half of the stop.** Called while the SSE connections are still open:
   - sets `closing` under `streamCapMu`: `tryStartStream` refuses from then on with `ErrServerRestarting`, which the
     handler answers with the same refusal (a request that passed the handler's check just before);
   - cancels every active stream with `ChatErrServerRestarting` (`SERVER_RESTARTING`, copy "response stopped: the
     server is restarting — try again in a moment"). `cancelWithCode` keeps the first reason with its code, so a
     stream the user stopped stays `STREAM_STOPPED`;
   - waits for `workers`, a `sync.WaitGroup` counting each stream's worker (`Add` in `tryStartStream`, under the
     same lock as `closing`, so no `Add` follows `Wait`; `Done` is the worker's first defer, so it runs after the
     terminal emit, the mirror hand-off and the answer persist) and its resume mirror (`workers.Go`; its final
     snapshot is written after the worker exits). The wait is bounded by ctx and `chatShutdownBudget` (2 s). A
     cancelled stream stores nothing, so in practice the wait is the cancellation reaching the provider call plus
     the persist of an answer that finished just before;
   - at the budget, every active stream that recorded no terminal state is ended in its stuck worker's place:
     `SERVER_RESTARTING` recorded, emitted to a begun stream's owner and mirrored. That worker may emit its own
     terminal event later; the client acts on the first. The error return says how many.
   - Logs, stable for the KQL alerts: INFO `chat streams ended for shutdown` (`streams`), WARN `chat workers still
     running at shutdown` (`streams`, `forced`, `budget`).
   `CancelAll()` stays as the non-waiting half (now `SERVER_RESTARTING` too); the stop hook calls `Shutdown`.
5. **SSE writes what is queued when it closes.** On `shutdownCh` the handler writes the frames already queued for its
   connection (under one 1 s write deadline) before it returns: with both the queue and the shutdown ready, its
   select would otherwise drop the terminal events at random.
6. **A resume of an unknown stream says so.** `/api/chat/resume` (and `begin`, `cancel`, `fix-decision`, which share
   the ownership check) answer code `STREAM_NOT_FOUND` for a stream the server does not know. The statuses are
   unchanged: 403 with accounts (an unknown stream and another user's are still not told apart), 404 without.
   Clients treat 403, 404 and 410 from a reconnect resume as "gone" and report the stream interrupted at once.
7. **The stop order (`internal/app`).** `registerDrain` is the last Invoke, so its stop hook runs first, before
   the jobs stop:

   ```go
   // app.go, at the end of the Invoke list:
   registerDrain,

   // server.go:
   func registerDrain(lc fx.Lifecycle, cfg *config.Config, chatSvc *service.ChatService) {
       lc.Append(fx.Hook{OnStop: func(ctx context.Context) error {
           if err := drain.Process().Drain(ctx, cfg.Runtime.ShutdownDrainDelay.Std(), chatSvc.Shutdown); err != nil {
               logger.Warn("drain incomplete", "error", err)
           }
           return nil // the rest of the stop must still run
       }})
   }
   ```

   `State.Drain(ctx, delay, end...)` is `Begin`, a pause of `delay` (cut short by ctx) so probes and load balancers
   see the 503 while requests still flow, then each `end` step in order, errors joined. The server's own hook no
   longer calls `chatSvc.CancelAll()`; it keeps `router.Shutdown()` first. `TestApp_StopOrder` pins the order and
   `TestApp_StopEndsChatStreamsBeforeSSECloses` runs it through the real graph: the terminal event reaches the open
   event stream, and the probes answer as below during the pause.

   **The budget.** `fx.StopTimeout` is 28 s, under a 35 s grace (ACA `terminationGracePeriodSeconds`, compose
   `stop_grace_period`; `internal/config/deploy_pool_test.go` keeps both above the timeout it reads from the
   wiring). At the timeout fx returns from `Stop` and the process exits: the hooks not yet run are skipped and the
   one running is killed with the process (`go.uber.org/fx` `withTimeout`, `lifecycle.Stop`). So the whole
   sequence, through the audit flush and the pool close at its end, must fit. The hooks in the order they run in the
   cloud, each with its bound:

   | # | Stop hook (registered by) | What it waits for | Bound | Spent when |
   |---|---|---|---|---|
   | 1 | `registerDrain` | the drain pause, then `ChatService.Shutdown` (workers, final snapshots, persists) | 3 s + 2 s | every stop; the 2 s only while a worker unwinds |
   | 2 | `scheduleSecretReseal`, `scheduleFieldEncryptionBackfill` | nothing: the passes are cancelled | 0 | |
   | 3 | `audit.ScheduleOrgBackfill` | a cancelled backfill run that does not end | 5 s | safety net |
   | 4 | `initScanner` | the cancelled sweep's unwinding, then the alert deliveries it started, together (`scanner.defaultStopBudget`; was 5 s + 5 s) | 5 s | a slow channel; a flow mid-analysis |
   | 5 | `startServer` | `router.Shutdown()` (each event stream writes what is queued, ≤ 1 s, inside the next wait); the WebSocket clients' close; then `server.Shutdown` and `lifecycle.DrainAll` on one context | 3 s + 8 s | slow socket clients; requests and background work in flight |
   | 6 | `initRetentionPurge` | a cancelled sweep that does not end | 5 s | safety net |
   | 7 | `initTelemetry` | the trace flush | 5 s | an OTLP collector that does not answer |
   | 8 | `api.NewRouter` | the hub's backplane | ~0 | |
   | 9 | `provideAuthManager` | the blacklist cache's reload and its cleanup job | 5 s | safety net |
   | 10 | `audit.Provide` | the audit flush | 5 s | a slow database (then spilled to disk) |
   | 11 | `provideStorageBackend` | the blob cleaner's drain, then the pool close | 15 s | safety net (a slow blob store) |
   | | `redisx` (when configured) | the client close | ~0 | |

   The stop as designed, every wait a healthy stop can spend at its bound: 3 + 2 + 5 + 3 + 8 = 21 s to the end of
   the server's step, then the tail in milliseconds and the audit flush at its own full 5 s: 26 s, inside 28 s.
   Without the scanner cap it was 31 s: the audit flush and the pool close were the steps the timeout cut (that was
   already so at 25 s). The usual stop is the pause plus about a second.

   The safety nets (a cancelled job that does not end, a dependency that does not answer, the blob cleaner's
   drain) each have a fixed budget of their own, not a share of the stop's deadline: one of them, hit alone, eats
   the 2 s of slack and the audit flush's; two together push the audit flush and the pool close past the timeout.
   Making them take their wait from the hook's context (`Singleton.Stop(ctx)`, `TickerLoop.Stop(ctx)`, the
   telemetry and blob cleaner shutdowns) is the remaining structural fix (Open). Also: every second of
   `PAD_SHUTDOWN_DRAIN_DELAY` above the cloud default comes out of the same 2 s; at its 10 s maximum the stop as
   designed no longer fits, so raise `StopTimeout` and the grace periods with a longer pause.

   This table is the one place the budget is written; `server.go`, `app.go` and `scanner.go` point here from
   their constants.

### The wire contract (agreed with the client change of this phase)

| Where | What the client gets |
|---|---|
| A running stream the stop ended | SSE `chat:event` `{"streamId":…,"type":"error","data":{"message":…,"code":"SERVER_RESTARTING"}}`, the shape of every stream error event, before the connection closes. The client keys on `code`. |
| `POST /api/chat/resume` or `/begin` of such a stream (same instance before it exits; any replica with Redis) | 200 with `"error":…,"errorCode":"SERVER_RESTARTING"` |
| `POST /api/chat/stream` while draining | 503, `Retry-After: 2`, envelope code `SERVER_RESTARTING`, `details.retryAfterSeconds: 2`. Safe to send again once after the wait. |
| `GET /api/events`, `GET /ws` while draining | the same 503 refusal; the client's reconnect backoff handles it |
| `GET /readyz`, `GET /api/health` while draining | 503 `{"status":"draining"}` |
| `POST /api/chat/resume` of a stream the server does not know | 403 (accounts) or 404, code `STREAM_NOT_FOUND`: the stream is gone |

### The client half (`frontend/src`, agent G)

The client codes against four points of the contract: the terminal event's `code` (the message is never shown; the
same code honoured in a resume or begin answer's `errorCode`); the refused create's envelope `code` with
`details.retryAfterSeconds` (positive, capped at 10 s, plus 0-1 s jitter; 2 s plus jitter without it, since
`ApiError` carries no response headers); 403, 404 or 410 from a reconnect's resume meaning gone; and
`STREAM_INTERRUPTED`, which the client produces itself for that case.

On `SERVER_RESTARTING` from either path the turn is sent again once, automatically, through the Retry path
(`lib/chatErrors.ts`, the stream engine's `scheduleAutoResend` and `send`): the partial text stays with a "the
server restarted, sending again" status line, no error bubble, and after the wait the question is re-sent with the
turn's own model and files and `includeHistory`, once the event stream is open again. One flag per turn covers both
paths, so a turn retried on create is not re-sent when its stream is then cut short, and a re-sent turn gets no
retry: no loop. Excluded, and shown the error bubble with Retry instead: a turn with a fix-proposal card (re-running
it is the user's call), a stream reattached after a reload (its question is not known), and a thread whose history
is not synced (the resend replays the local history). A reconnect whose resume finds a live stream gone reports it
interrupted at once (the fast path), instead of the stall probe's 90-120 s; no automatic resend there, since the
client cannot know it was a deploy.

## Consequences

- A deploy with an open chat: the client hears `SERVER_RESTARTING` before its connection drops and sends the turn
  again on the replica its reconnect reaches, instead of "stalled" a minute and a half later. A client of the
  previous release shows it as a generic error with its Retry, worded "the server is restarting — try again in a
  moment"; it shows a refused create the same way.
- An answer that finished just before the stop is stored before the pool closes, and the last resume snapshot
  carries the terminal code before Redis closes (each within the 2 s budget).
- A stopping replica leaves rotation on its first readiness probe: compose's `healthcheck`, other load balancers and
  the old revision of a swap see it. On ACA the platform stops routing on termination anyway. The readiness alert
  (`infra/main.bicep`, `docs/architecture/operations.md`) counts per replica and needs more failures than a
  replica can log in its grace period, so a deploy's draining 503s do not fire it: settled by item D.
- A refused WebSocket upgrade leaves its ticket unspent.
- A refused request is never logged at ERROR, and its 503 message is shown as written.
- Stopping takes `PAD_SHUTDOWN_DRAIN_DELAY` longer (3 s in the cloud); the platform waits up to 35 s (was 30 s on
  ACA, 10 s under compose).
- What an operator sees in a deploy: `server draining`, the readiness probe 503 for the pause, `chat streams ended
  for shutdown` with the count, `shutting down`, then the usual closes; `drain incomplete` or `chat workers still
  running at shutdown` (alerted) only when a worker ignored its cancellation.
- The turn a stopped stream was answering is not stored (a cancelled stream stores nothing; its question was stored
  at start). The automatic resend asks it again.

## Rollback

No schema, no configuration and no stored format change: the resume snapshot keeps its fields (`errorCode` already
existed). The previous release ignores all of this and stops as before. During a rolling deploy a previous-release
replica relays a snapshot's `SERVER_RESTARTING` as any other code. A client of this phase against the previous
backend never sees `SERVER_RESTARTING` and gets 403 `FORBIDDEN`/404 `NOT_FOUND` for an unknown stream, which it
treats as gone by status; a client of the previous release against this backend switches on no `FORBIDDEN` there
(checked), so `STREAM_NOT_FOUND` changes nothing for it.

## Open

- The safety-net waits of the budget table take fixed budgets of their own instead of a share of the stop's
  deadline (see the table); the trace flush also runs before the audit flush and the pool close.
- No metric counts refusals or streams ended by a stop; the logs above carry the counts.
- A resume cannot tell "this server restarted" from "unknown stream": a boot id in a response header would let the
  client say `SERVER_RESTARTING` instead of interrupted without a reconnect in between.
- A worker stuck past the budget can emit a second terminal event after the forced one.
