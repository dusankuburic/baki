# 43. A turn is stored whole or not at all, and every turn leaves a record

Date: 2026-10-26. Status: accepted, implemented (AI roadmap step 8). Extends ADR 0031 (server-owned history) and ADR 0024 (graceful shutdown).

## Context

A server-owned turn writes the conversation twice: the question at the start and the answer at the end. Either write could fail on its own, and in each case the thread was left wrong:

- A history that could not be read was answered anyway, without its context.
- A question whose write lost every compare-and-swap round, or hit a database error, left the answer stored under the previous answer: two answers in a row, with no question between them.
- A failed answer write was logged and dropped. The client had been told the server owned the turn, so it saved nothing either.

The answer was also stored inside the chunk coalescer's emit gate. A slow store therefore held up the heartbeat and the batch flush behind it, and a panicking delivery left the gate locked. At shutdown, an answer write still running when the budget ran out kept its stream past the point where the connections close, so the client never got a terminal event.

Once a turn ended, nothing recorded how. There was no log line, no counter and no duration. The idle watchdog's cancellations and the disconnect's were reported as the user's own stop. Chat and provider log lines used the package-level logger, so none of them carried the request, user, trace or stream.

## Decision

1. **No history, no turn.** A history that cannot be read ends the turn with `HISTORY_UNAVAILABLE` (retryable) before any provider call.
2. **The question travels with its answer.** If the question's write at the start fails, the answer's write stores the question too, right before the answer. Each such recovery is counted under `answer_with_question` in `pad_chat_conversation_conflicts_total`. The start's compare-and-swap backoff grows with each round.
3. **A failed answer write is retried, counted and announced.**
   - The answer write gets up to three attempts within `persistAnswerTimeout`. A lost compare-and-swap round is never retried here.
   - When the write still fails, `pad_chat_answer_write_failures_total{cause}` counts it, and the done event carries `answerStored: false`. The client then saves the turn itself, as it does for a turn it owns.
   - The field is absent when the write succeeded, so older clients see the done event unchanged.
4. **The answer is stored outside the emit gate.**
   - The done is claimed as the stream's terminal event first, then the answer is stored, then the done is emitted.
   - The gate is released even when a delivery panics.
   - The worker's cleanup runs each step best-effort, so a panic cannot skip releasing the stream's slot.
5. **The stop cuts what is left.** When Shutdown's budget runs out, it closes the service's stop channel.
   - An answer write that is still waiting, on the store or on its conversation's stripe lock, gives up with cause `timeout`.
   - Its worker sends the done it already claimed, with `answerStored: false`.
   - Shutdown waits a further 500 ms for those workers.
6. **Every turn leaves one record.** One `chat turn ended` log line with:
   - the outcome and its code;
   - provider, requested and served model;
   - tokens, iterations, tool calls, and approvals by status;
   - duration.

   With it come `pad_chat_turns_total{outcome}`, `pad_chat_turn_duration_seconds` (whose top bucket covers the 10-minute cap), `pad_chat_tool_duration_seconds{tool}` and `pad_chat_approval_wait_seconds{status}`.
   - The worker's cleanup writes the record, or Shutdown does for a stream it ended in a stuck worker's place, whichever comes first.
   - The outcome is `done`, or the lowercased failure code the client saw. A user's stop is `stopped`; the watchdog's cancellations are `idle_timeout` and `subscriber_gone`. All three sent `STREAM_STOPPED` on the wire at first; R-streaming-10 gave the watchdog's two their own codes, `STREAM_IDLE_TIMEOUT` (its message names the phase that stalled) and `STREAM_CLIENT_GONE`, with the same outcome labels.
7. **Chat and provider lines carry their context.**
   - These lines go through `logger.FromContext`.
   - `logger.WithAttrs` puts the stream's `stream_id` on the worker's context, next to the request's `request_id`, `user_id` and `trace_id`.
   - An upstream rejection is logged whether or not the vendor sent a request id.
   - `TestArch_BareChatLogCallsRatchet` pins the calls left without a context.

The client half (step 8 frontend):
- Error lines are left out of the history a request sends.
- Closing a thread deletes its stored conversation, on the version the client holds. On a 409 the thread stays open.
- A thread closed mid-stream is abandoned without any client save, including a fix card being applied; its flow change keeps its audit row. The server does not write the stopped turn of a conversation cleared while its stream ran.
- A thread the thread cap evicts is deleted too.
- A backend restart ends the client's live streams and points it at the new process.
- The events stream reconnects without a give-up and shows a disconnected state.

## Consequences

- The stored thread holds a turn's question and answer together, or neither. The remaining gap is a turn whose answer write fails while the client is gone. That turn has no answer until the client returns and its own save lands. It is counted and alertable.
- An operator can answer "what happened to this turn" from one log line, joined to the access log and the trace by `request_id` and `trace_id`. They can also answer "how many turns failed with this code this hour" from one counter.
- A shutdown that hits its budget takes up to 500 ms longer.
- The idle and disconnect outcomes are operator labels, not client codes. A client that wants to tell them apart needs a wire code of its own, which is a later change.
