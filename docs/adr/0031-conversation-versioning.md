# 31. Optimistic versioning of persisted conversations

Date: 2026-10-13. Status: accepted, implemented (Phase 5b, multi-replica).

## Context

A conversation is one row of `conversations (flow_id, scope, messages JSONB)` holding the whole message list, and every
writer replaced the whole list: `SaveConversation` is `INSERT ... ON CONFLICT DO UPDATE` with no condition. Four writers
share a row:

- **W1**, the user turn at stream start: read the history (it becomes the model's context), append the question, write.
- **W2**, the assistant answer at stream end (`persistAssistantTurn`): read, insert the answer by message id, write.
- **W3**, the client's save (`POST /api/chat/save`): write the client's list, whatever the row holds.
- **W4**, the clear (`POST /api/chat/clear`): delete.

W1 and W2 were serialised by a striped in-process mutex (256 stripes). That is correct for one process and nothing
more. With several replicas (Phase 5b) the later writer of two overlapping read-modify-writes erases the earlier one's
message: W1 against W1 (two tabs, two replicas), W2 against W3 (the client's list lacks what the server added, or the
server's answer is written over the client's), W2 against W2, and a clear racing a persist that resurrects the
history. W3 was unguarded even on one replica: no mutex and no version. During a rolling deploy old and new binaries
write the same rows, and an old client cannot send a version.

## Decision

1. **A version on the row, assigned by the database.** Migration 0044 (`expand`) adds
   `conversations.version BIGINT NOT NULL DEFAULT 0`, a sequence `conversation_version_seq` and a trigger
   `BEFORE INSERT OR UPDATE` that sets `version := nextval(...)` on every insert and on an update that changes
   `messages`. The default is a constant, so the column is metadata only: no rewrite, however large `conversations`
   is (a `nextval` default would rewrite the table). Rows that exist when the migration runs keep version 0
   ("legacy") until their messages change.
   - **A trigger, not an application `version + 1`.** The previous release's blind upsert names no version column and
     still bumps it, so a compare-and-swap holder of this release sees a write of the old binary. That is what makes
     the step safe in a rolling deploy.
   - **One sequence for all rows, not a per-row counter, and the insert trigger too: no ABA after delete and
     re-create.** A conversation that is cleared and started again is a new row, and it gets a number nobody had. With
     `version + 1` per row a recreated row would count 1, 2, 3 again and a holder of the old version 3 would match it.
     Here a created row is never 0 and never equals any earlier version of any row, so the old holder conflicts.
   - **The residual ABA is the legacy 0.** "Does not exist" and "legacy, never changed since the migration" are both 0
     and both mean "create or replace with expected 0". A writer that read a legacy row, saw it cleared, and then wrote
     with expected 0 re-creates it. That is the resurrection the old code did unconditionally (W2 after a clear writes
     the answer into the empty list), it needs a conversation untouched since the deploy, and it ends with the first
     write of each legacy row. If a deployment wants it gone sooner, run once, in batches, outside any migration:
     `UPDATE conversations SET version = nextval('conversation_version_seq') WHERE (flow_id, scope) IN (SELECT flow_id, scope FROM conversations WHERE version = 0 LIMIT 500)`, until it updates no row (it changes only `version`, which the trigger leaves alone).
   - An update that leaves `messages` alone (the same list saved again, or a different `updated_at` only) keeps the
     version, as `SaveConversation`'s `IS DISTINCT FROM` already kept `updated_at`.
2. **Storage.** `ConversationStore` gains `LoadConversationVersioned` (a missing conversation is `[]` at 0),
   `SaveConversationIf` (compare-and-swap; returns the new version, or `ErrConversationConflict` together with the
   current version) and `DeleteConversationIf`. `ErrConversationConflict` wraps `ErrVersionConflict`, so the generic
   mapping to 409 applies. The old three methods stay for the migrator and for callers with no version to hold. The
   Postgres swap is the `UPDATE ... WHERE version = $expected RETURNING version` itself (under READ COMMITTED a writer
   that waited for the row lock re-checks the predicate), and a create with expected 0 is
   `INSERT ... ON CONFLICT DO NOTHING RETURNING version`. The fake backend counts the same way. Scenarios in
   `storage/contract/conversations_version.go` run on both: missing is `[]` at 0, create expects 0, a stale expected
   version conflicts and writes nothing, eight concurrent writers on one base give exactly one winner, versions rise
   and survive delete and re-create, a blind save invalidates a held version and an unchanged one does not, delete
   with a stale or current version, and `errors.Is(err, ErrVersionConflict)`.
3. **`ChatService.mutateConversation` for W1 and W2.** It loads with the version, applies an idempotent function to
   the list, saves with the swap and, on a conflict, loads again and re-applies, up to five times with a 10 to 50 ms
   jitter, then gives up with `ErrConversationConflict` (W1 and W2 log it; the stream goes on, as it did when a save
   failed). W1 decides its message id from the history it is given (a resend that reuses a stored id mints a fresh
   one), W2 skips when its answer id or an equal trailing answer is already there, as before. W1 still hands the model
   the history it read. Cloud mode only: the file path of local modes keeps its single round.
   - **The in-process mutex stays**, as a queue and not as the correctness mechanism. It keeps one replica's writers to
     one contender at a time, so the swap only has to settle replicas against each other and five rounds suffice for
     a handful of replicas (without it, 16 writers on one replica would need up to 16 rounds). It is also what makes
     the file path correct, and its panic and timeout behaviour is pinned by tests.
4. **API.** `POST /api/chat/get` returns `version` (omitted where conversations are files). `POST /api/chat/save` takes
   an optional `baseVersion`: when present the list is stored only if the conversation is still at it, and the answer
   is `{status, version}`; a stale one is 409 `CONVERSATION_CONFLICT` with `details.version`, and nothing is written.
   `POST /api/chat/clear` takes the same field (`DeleteConversationIf`). **Without `baseVersion` the save stays the
   blind replace it was** (old clients, rolling deploys) and is counted in `pad_chat_blind_saves_total`, so the
   contract step can be scheduled when the counter has stayed at zero.
5. **Client.** The chat store keeps the version per thread inside the thread's history status. A new thread starts at 0,
   a load stores what GET returned, a save stores what it answers, a clear resets to 0. A save sends `baseVersion`
   whenever one is known. On `CONVERSATION_CONFLICT` a save reloads, merges by message id (the server's list in its
   order, a message only this client has kept after the one it followed), shows the merged list and saves once more on
   the new version, unless the server already holds every message; a second conflict is the existing "could not save"
   toast. Compaction and clear are not retried: the client takes the server's list and says the conversation changed
   elsewhere. A server that sends no version leaves the thread unversioned and its saves blind, as before.

## Consequences

- No turn is lost between two replicas or between the server's persist and the client's save; the lost-update windows of
  W1, W2 and the versioned W3 and W4 are closed. Blind W3 and W4 (old clients) are still racy, now visibly counted.
- The client's own save after a turn conflicts as a rule, not as an exception: the server wrote the turn's question and
  answer itself, so the version the client holds is already stale when it saves. The conflict costs one reload and,
  because the server already holds every message, no second write. This is the price of keeping the client save
  (it still carries error bubbles' removal and the legacy path) without making it blind; see Open.
- `conversations` gains an 8-byte column and a trigger on insert and update. The trigger is a few instructions and one
  `nextval` per changed write; nothing reads the column except the swap and GET.
- A conflict exhausting its five rounds loses the turn it was writing (logged at WARN). That needs six writers on one
  thread landing inside one another's read-write window; not seen outside the contract test with no mutex.
- The fake backend, the contract suite and the Postgres backend implement the same three methods. Test wrappers around
  the fake that intercept `SaveConversation` now intercept `SaveConversationIf` too.

## Rollback

Expand step, no data change. The previous release ignores the column and the trigger fires on its writes, so its blind
saves bump the version and a new replica's swap sees them. `bakicli migrate down 43` drops the trigger, the function,
the column and the sequence. Mixed releases work both ways: an old client against a new server sends no `baseVersion`
(blind, counted); a new client against an old server sends a `baseVersion` the old server ignores and gets no version
back, so its saves stay unconditional; a client that learned a version and meets a server that was then rolled back
simply stops getting versions. Replicas of either release can serve the same row.

## Open

- **The contract step.** When `pad_chat_blind_saves_total` has stayed at zero, `baseVersion` can become required on
  save and clear and the blind path can go.
- **The per-turn conflict.** Done: see the addendum.
- **The legacy 0.** The optional backfill above. Not run by the migration, because it rewrites every row.
- **A conflict on clear for a conversation that is gone** answers 409 when the client held a version, and the client
  reloads an empty list. That is correct and slightly noisy.
- `DeleteConversationIf` returns no current version; the service reads one more time for `details.version`.

## Addendum (Phase 5b review round): the stored version leaves with the stream

The review of the first cut (database review H1/M1/M2, compatibility review M2) found that the client could not learn
the version the server's own writes left: it held the version it loaded, every later client write on it was stale, and
the repair (a 409, a reload, a merge, one retry) had three faults. This addendum records the fixes; the decisions above
stand.

- **The premise, corrected.** "The client's own save after a turn conflicts as a rule" was imprecise: a normal
  server-owned turn never saved on `done` (`save: !acc.serverOwned`). What conflicted was every client write that
  followed: a stopped turn's save, a stream re-attached after a reload (the ownership flag is not persisted), the
  re-send of a turn the server had begun, and the next compaction or clear on the version held since the load, which
  was stale after any streamed turn.
- **The answer is stored before `done`, and `done` names the version.** The server-owned turn's answer write moved
  from the worker's cleanup into the emit of `done` (it needs the journal and the billed rates the event itself
  records, so it runs there, under the same order that already flushes the last chunk first). The event gains
  `conversationVersion` (the version after the answer write) and `conversationBase` (the version the question write
  read), both additive. They are sent only when the two server writes were adjacent: the answer's winning round read
  exactly the version the question's write returned. If another writer got in between, the version covers a message
  the client has not seen, so neither field is sent and the client keeps its own version. An `error` event carries the
  question's pair (the question was stored at stream start), so a failed turn does not leave the client stale either.
  The cleanup still stores the answer for the streams that end without a `done` emit and does not repeat a write that
  ran. Cost: `done` waits for the answer write (bounded by `persistAnswerTimeout`), and the stream stays in the
  active set until it has run; the cap on concurrent streams per user counts it a few milliseconds longer.
- **The client adopts only from the right base.** `adoptStreamVersion`: when the thread still holds
  `conversationBase`, it takes `conversationVersion` and skips any save for the turn (also for a re-attached stream);
  otherwise it keeps its version and the next write reconciles. Adopting blindly would let the next compaction
  overwrite a tab's message the client never saw, which is what the compare-and-swap exists to prevent.
- **`mergeHistory` keeps the server-trimmed head in front.** The server keeps the newest 200 messages (1 MiB); local
  messages before the first one it still holds were moved behind the whole list and re-saved as the newest. They now
  stay where they were. A save that includes them is trimmed back to the same list by the server.
- **The reconcile retry is a bounded loop and ends quietly.** Up to three save rounds; when the thread keeps changing,
  the client still adopts the server's latest version (and keeps its unsaved messages for the next save) and shows no
  "could not save" toast. A following clear or compaction is made on the adopted version and does not conflict.
- **A re-sent turn keeps its question's id.** The automatic re-send after a server restart removed the question the
  server had already stored and asked again under a new id, so the reconcile brought the old question back after the
  new answer. It now reuses the id, and the error event's pair lets it save on the stored version without a conflict.
- **The user turn is stored once.** When the request's question id is already in the stored thread (a late client save
  of the same turn, a retried round), `persistUserTurnVersioned` writes nothing and the model's context ends before
  it. The file path is unchanged.
- **A lost write is counted.** `mutateConversation` exhausting its rounds increments `pad_chat_persist_giveups_total`
  and logs a short hash of the conversation (flow and key), not the key.
- **Import of a flow's legacy conversation and `markHistoryLoaded` keep the thread's version.**

Compatibility: both fields are new keys in an existing payload (the SPA before this change ignores them; a server before
it sends none and the client keeps its old behaviour of saving or reconciling). Rolling deploy: an old replica's `done`
carries no version and a new client falls back to the reconcile path, which is now bounded and quiet. Rollback is a
binary rollback; no schema is involved.

Not covered: a client that missed the live `done` and learns the end of the turn from a resume snapshot gets no version
(the snapshot does not carry the pair), so it reconciles once as before. The reconcile still merges additively, so a
conversation cleared in another tab comes back when an idle tab saves (the old blind save did the same).
