package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/apperr"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// Optimistic versioning of persisted conversations (ADR 0031). In cloud mode a conversation row has a version the
// database moves on every change; the server's own writers (the user turn at stream start, the assistant answer at
// stream end) read it, change the list and write with a compare-and-swap, and a client save may do the same with the
// version it loaded. Local modes keep files and the process-local lock, and have no versions.

// CodeConversationConflict is the machine code of the 409 a stale client save or clear gets; details.version is the
// conversation's current version.
const CodeConversationConflict = "CONVERSATION_CONFLICT"

// CodeConversationKeyTaken is the machine code of the 409 a save or a title gets on a key that holds another user's
// conversation (R-security-3): no reload makes it succeed; the client starts the thread under a new key.
const CodeConversationKeyTaken = "CONVERSATION_KEY_TAKEN"

// CodeConversationUnowned is the machine code of the 403 a clear (or a title) gets on a conversation stored before
// conversations had owners, from a caller that may not delete the flow: nobody can tell whose it is. The client
// drops the thread from its own list; the conversation goes with its flow.
const CodeConversationUnowned = "CONVERSATION_UNOWNED"

// mutateConversationAttempts bounds a writer's read-modify-write rounds. A round is lost only to another writer that
// finished its own first, so each lost round is someone else's progress; five covers a handful of replicas writing
// one thread at once, and the per-conversation lock keeps the contenders of one replica to a single at a time.
const mutateConversationAttempts = 5

// mutateConversationBackoff is the pause before round attempt+1: short and jittered so replicas that lost together
// do not meet again. A test variable.
var mutateConversationBackoff = func(attempt int) time.Duration {
	// Grows with the round, so several writers on one hot thread (replicas, a tab saving on this replica) cannot
	// use every round up within a few tens of milliseconds.
	return time.Duration(attempt) * (10*time.Millisecond + rand.N(40*time.Millisecond)) // #nosec G404 -- jitter only, not security-sensitive
}

// chatBlindSavesTotal counts client saves that carried no base version, which the server must apply as the blind
// full replace it always was. It falls to zero as old clients go away; when it has stayed there, the optional
// baseVersion can become required.
var chatBlindSavesTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "pad_chat_blind_saves_total",
	Help: "Client conversation saves (POST /api/chat/save) that sent no baseVersion in cloud mode, applied as a blind full replace that can overwrite another writer's turn. Falls to zero as old clients go away; the optional baseVersion can become required once it has stayed at zero.",
})

// chatPersistGiveUpsTotal counts server-side conversation writes (the question at stream start, the answer at stream
// end) that lost every compare-and-swap round and were dropped. The answer then lives only in a still-open tab; a
// steady rate is a hot thread written from several replicas faster than the retry bound absorbs.
var chatPersistGiveUpsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "pad_chat_persist_giveups_total",
	Help: "Server conversation writes (a turn's question at stream start, its answer at stream end) that lost every compare-and-swap round to concurrent writers and were dropped; the answer then exists only in a still-open client. A steady rate means one thread is written from several replicas faster than the retry bound absorbs.",
})

// chatConversationConflictsTotal counts every lost compare-and-swap on a conversation by where it happened, so
// contention on shared threads is visible before it costs a turn: client_save and client_clear are the 409
// CONVERSATION_CONFLICT answers (a tab saved or cleared over a version it had not seen), retry a server write's round
// that lost and runs again on the fresh history, retry_exhausted a server write that lost every round (also counted in
// pad_chat_persist_giveups_total).
var chatConversationConflictsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pad_chat_conversation_conflicts_total",
	Help: "Lost compare-and-swaps on a stored conversation, by source: client_save and client_clear (answered 409 CONVERSATION_CONFLICT: the client reloads, merges and saves again), retry (a server write's round lost to another writer and run again on the fresh history), retry_exhausted (a server write lost every round and was dropped, as pad_chat_persist_giveups_total). A rising retry rate is one thread written from several replicas or tabs at once.",
}, []string{"source"})

// conversationConflictSources is the closed value set of the source label. answer_with_question is a turn whose
// question was not stored at its start (every round lost, or the write failed): its answer's write stores the
// question with it, so the thread holds both or neither.
var conversationConflictSources = []string{"client_save", "client_clear", "retry", "retry_exhausted", "answer_with_question"}

// chatAnswerWriteFailuresTotal counts server-owned answers whose write at the end of the turn failed after its
// retries: the done event then says answerStored false and the client saves the turn itself.
var chatAnswerWriteFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pad_chat_answer_write_failures_total",
	Help: "Server-owned chat answers whose write at the end of the turn failed after its retries, by cause: error (the store refused or failed), timeout (persistAnswerTimeout ran out, or the server's stop cut it), conflict (every compare-and-swap round lost). A done turn's done event says answerStored false and the client saves the turn; a failed turn's write, made after its error event, is counted but cannot be announced.",
}, []string{"cause"})

func init() {
	metrics.MustRegister(chatBlindSavesTotal, chatPersistGiveUpsTotal, chatConversationConflictsTotal, chatAnswerWriteFailuresTotal)
	// Every source is exported from the first scrape, so a rate over it starts at zero instead of absent.
	for _, src := range conversationConflictSources {
		chatConversationConflictsTotal.WithLabelValues(src)
	}
	for _, cause := range []string{"error", "timeout", "conflict"} {
		chatAnswerWriteFailuresTotal.WithLabelValues(cause)
	}
}

// conversationLogID is what a log line says about a conversation: a short hash of its flow and key, enough to
// correlate lines of one hot thread without writing the (user-chosen) key or the flow id.
func conversationLogID(flowID, key string) string {
	sum := sha256.Sum256([]byte(flowID + "\x00" + key))
	return hex.EncodeToString(sum[:6])
}

// convWrite is a server write of a conversation seen from the client: the version the history was read at and the
// version the write left behind. A stream's terminal event carries it (conversationBase, conversationVersion) so a
// client that held Base can move to Version without a round trip.
type convWrite struct{ Base, Version int64 }

// conversationMutation derives a conversation's new list from the one stored. It may run several times, each on a
// fresh read, so it must be idempotent: decide from the history it is given (a message id already there means the
// work is done), and report changed=false then.
type conversationMutation func(history []models.ChatMessage) (out []models.ChatMessage, changed bool, err error)

// mutateConversation applies fn to the stored conversation and stores the result, and reports whether it did
// (false with a nil error: nothing to change, or "Save conversation history" is off).
//
// In cloud mode the write is a compare-and-swap on the version read with the history: when another writer got in
// between (another replica, or a client save), fn runs again on what that writer left, so one writer's turn never
// replaces the other's. After mutateConversationAttempts lost rounds it gives up with ErrConversationConflict.
// Local modes have one process and its file lock, so one round is all they need.
//
// The callers still hold the per-conversation mutex. It is no longer what keeps the data right, but it queues the
// writers of one replica, so the compare-and-swap only has to settle replicas against each other.
func (s *ChatService) mutateConversation(ctx context.Context, flowID, key string, fn conversationMutation) (bool, error) {
	res, err := s.mutateConversationAt(ctx, flowID, key, fn)
	return res.Changed, err
}

// mutation is what mutateConversationAt did. Loaded is the version of the round that decided the outcome (the one that
// wrote, or the last one read), Version the version the conversation is at afterwards, and Versioned says both mean
// something (false for file storage and for history off).
type mutation struct {
	Changed         bool
	Loaded, Version int64
	Versioned       bool
}

// mutateConversationAt is mutateConversation reporting the versions too.
func (s *ChatService) mutateConversationAt(ctx context.Context, flowID, key string, fn conversationMutation) (mutation, error) {
	if !s.useBackendConversations() {
		history, err := s.GetConversation(ctx, flowID, key)
		if err != nil {
			return mutation{}, err
		}
		out, changed, err := fn(history)
		if err != nil || !changed {
			return mutation{}, err
		}
		stored, err := s.saveConversation(ctx, flowID, key, out)
		return mutation{Changed: stored}, err
	}
	for attempt := 1; ; attempt++ {
		stored, version, err := s.backend.LoadConversationVersioned(ctx, flowID, key, callerScope(ctx))
		if err != nil {
			return mutation{}, err
		}
		out, changed, err := fn(toModelMessages(stored))
		if err != nil {
			return mutation{}, err
		}
		if !changed {
			return mutation{Loaded: version, Version: version, Versioned: true}, nil
		}
		if !s.conversationHistoryEnabled(ctx) {
			return mutation{}, nil
		}
		written, err := s.backend.SaveConversationIf(ctx, flowID, key, callerScope(ctx), version, toStorageMessages(normalizeConversation(callerScope(ctx), out)))
		if err == nil {
			return mutation{Changed: true, Loaded: version, Version: written, Versioned: true}, nil
		}
		if !errors.Is(err, storageif.ErrConversationConflict) {
			return mutation{}, err
		}
		if attempt >= mutateConversationAttempts {
			chatPersistGiveUpsTotal.Inc()
			chatConversationConflictsTotal.WithLabelValues("retry_exhausted").Inc()
			logger.FromContext(ctx).Warn("chat: gave up a conversation write after losing every round to concurrent writers",
				"conversation", conversationLogID(flowID, key), "attempts", attempt)
			return mutation{}, err
		}
		chatConversationConflictsTotal.WithLabelValues("retry").Inc()
		if d := mutateConversationBackoff(attempt); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return mutation{}, ctx.Err()
			}
		}
	}
}

// stampConversationVersion adds the stored version to a done or error event: the client held Base when the turn began
// and holds Version now, so it may adopt it when its own version is still Base (nothing happened to the thread that it
// has not seen). Absent when this stream did not write the conversation or someone else wrote during the turn.
func (c *streamCtl) stampConversationVersion(data map[string]interface{}) {
	if w := c.convWrite.Load(); w != nil {
		data["conversationVersion"] = w.Version
		data["conversationBase"] = w.Base
	}
}

// ConversationSaved is what a client save reports: the new version when the store has versions.
type ConversationSaved struct {
	Version   int64
	Versioned bool
}

// SaveClientConversation is the client's save (POST /api/chat/save). With base it is a compare-and-swap: the list is
// stored only if the conversation is still at the version the client loaded, else a 409 CONVERSATION_CONFLICT carries
// the current version. Without base (a client from before versions) it is the blind full replace, counted. File
// storage has no versions, so base is ignored there.
func (s *ChatService) SaveClientConversation(ctx context.Context, flowID, provider string, base *int64, messages []models.ChatMessage, mayReplaceUnowned bool) (ConversationSaved, error) {
	if flowID == "" {
		return ConversationSaved{}, fmt.Errorf("no flow loaded")
	}
	if !s.useBackendConversations() {
		_, err := s.saveConversation(ctx, flowID, provider, messages)
		return ConversationSaved{}, err
	}
	// A shared conversation (stored before owners under "flow" or a block id, R-security-3) belongs to nobody: the
	// flow's chat audience appends to it through their turns, but replacing its list — which can drop everyone else's
	// messages — needs the right to delete it. Ownership never moves to such a row, so the check cannot go stale.
	if owner := callerScope(ctx); owner != "" && !mayReplaceUnowned {
		shared, err := s.backend.IsSharedConversation(ctx, flowID, provider)
		if err != nil {
			return ConversationSaved{}, err
		}
		if shared {
			return ConversationSaved{}, conversationOwnerError(storageif.ErrConversationUnowned)
		}
	}
	if base == nil {
		chatBlindSavesTotal.Inc()
		_, err := s.saveConversation(ctx, flowID, provider, messages)
		return ConversationSaved{}, conversationOwnerError(err)
	}
	// "Save conversation history" off: accepted and dropped, the version unchanged.
	if !s.conversationHistoryEnabled(ctx) {
		return ConversationSaved{Version: *base, Versioned: true}, nil
	}
	version, err := s.backend.SaveConversationIf(ctx, flowID, provider, callerScope(ctx), *base, toStorageMessages(normalizeConversation(callerScope(ctx), messages)))
	if errors.Is(err, storageif.ErrConversationConflict) {
		chatConversationConflictsTotal.WithLabelValues("client_save").Inc()
		return ConversationSaved{}, conversationConflict(err, version)
	}
	if err != nil {
		return ConversationSaved{}, conversationOwnerError(err)
	}
	return ConversationSaved{Version: version, Versioned: true}, nil
}

// ClearConversationIf deletes the conversation only while it is at the version the client loaded; a changed one is
// the same 409 CONVERSATION_CONFLICT as a stale save. File storage has no versions and clears as ClearConversation.
// Only the caller's own conversation is deleted (R-security-3): another user's under the key is a missing one to the
// caller, and an unowned (pre-owner) one needs mayDeleteUnowned (the caller may delete the flow), else 403
// CONVERSATION_UNOWNED.
func (s *ChatService) ClearConversationIf(ctx context.Context, flowID, provider string, base int64, mayDeleteUnowned bool) error {
	if flowID == "" {
		return fmt.Errorf("no flow loaded")
	}
	if !s.useBackendConversations() {
		return s.ClearConversation(ctx, flowID, provider, mayDeleteUnowned)
	}
	err := s.clearingConversation(ctx, flowID, provider, func() error {
		return s.backend.DeleteConversationIf(ctx, flowID, provider, callerScope(ctx), base, mayDeleteUnowned)
	})
	if !errors.Is(err, storageif.ErrConversationConflict) {
		return conversationOwnerError(err)
	}
	chatConversationConflictsTotal.WithLabelValues("client_clear").Inc()
	// Only a hint for the client: when the read fails it learns the version by reloading.
	_, current, lerr := s.backend.LoadConversationVersioned(ctx, flowID, provider, callerScope(ctx))
	if lerr != nil {
		logger.FromContext(ctx).Debug("chat: could not read the current conversation version for a conflict", "error", lerr)
	}
	return conversationConflict(err, current)
}

// conversationConflict is the API error of a lost compare-and-swap.
func conversationConflict(cause error, current int64) error {
	return apperr.Conflict(CodeConversationConflict, "this conversation changed since you loaded it; reload it and try again").
		WithDetails(map[string]any{"version": current}).
		Wrap(cause)
}

// conversationOwnerError is the API error of a write the owner rule refused (R-security-3): a key that holds another
// user's conversation (409 CONVERSATION_KEY_TAKEN), a delete or a name of an unowned one the caller may not make
// (403 CONVERSATION_UNOWNED). Any other error is returned as it is.
func conversationOwnerError(err error) error {
	switch {
	case errors.Is(err, storageif.ErrConversationKeyTaken):
		return apperr.Conflict(CodeConversationKeyTaken, "this conversation key belongs to another user's conversation; start a new thread").Wrap(err)
	case errors.Is(err, storageif.ErrConversationUnowned):
		return apperr.Forbidden(CodeConversationUnowned, "this conversation was stored before conversations had owners; only someone who may delete the flow can delete it").Wrap(err)
	}
	return err
}

// ConversationRead is a stored conversation as GET returns it: the messages, the caller's stream whose answer is not
// stored yet, and the version the messages were read at (Versioned is false for file storage).
type ConversationRead struct {
	Messages       []models.ChatMessage
	ActiveStreamID string
	Version        int64
	Versioned      bool
}

// ReadConversation is ConversationWithActiveStream plus the version, read together with the messages.
func (s *ChatService) ReadConversation(ctx context.Context, scope, flowID, convKey string) (ConversationRead, error) {
	var ctl *streamCtl
	var remote *remoteStream
	if flowID != "" {
		if ctl = s.activeStreamCtl(scope, flowID, convKey); ctl == nil {
			remote = s.remoteActiveStream(ctx, scope, flowID, convKey)
		}
	}
	out := ConversationRead{}
	var err error
	if s.useBackendConversations() && flowID != "" {
		var stored []storageif.ChatMessage
		if stored, out.Version, err = s.backend.LoadConversationVersioned(ctx, flowID, convKey, callerScope(ctx)); err != nil {
			return ConversationRead{}, err
		}
		out.Messages, out.Versioned = toModelMessages(stored), true
	} else if out.Messages, err = s.GetConversation(ctx, flowID, convKey); err != nil {
		return ConversationRead{}, err
	}
	switch {
	case ctl != nil:
		if !answerStored(ctl, out.Messages) {
			out.ActiveStreamID = ctl.streamID
		}
	case remote != nil && !remote.answerStored(out.Messages):
		out.ActiveStreamID = remote.id
	}
	return out, nil
}

// newUserTurn is the message that stores the request's question after history.
func newUserTurn(req models.ChatRequest, history []models.ChatMessage) models.ChatMessage {
	userID := clientMessageID(req.UserMessageID)
	// A resend reusing a stored message id gets a fresh one, not a duplicate in the history.
	if slices.ContainsFunc(history, func(m models.ChatMessage) bool { return m.ID == userID }) {
		userID = uuid.NewString()
	}
	return models.ChatMessage{
		ID:   userID,
		Role: "user",
		// As typed; the write stores its scrubbed copy (normalizeConversation, through the history memo).
		Content:        req.UserMessage,
		Timestamp:      time.Now(),
		ContextBlockID: req.ContextBlockID,
	}
}

// storedUserTurn is where the request's question already sits in history (-1: not stored). The question is stored
// under the client's id, so a late client save of the same turn, or a round that follows a lost one, finds it.
func storedUserTurn(req models.ChatRequest, history []models.ChatMessage) int {
	id := req.UserMessageID
	if id == "" || clientMessageID(id) != id {
		return -1
	}
	return slices.IndexFunc(history, func(m models.ChatMessage) bool { return m.ID == id && m.Role == "user" })
}

// persistUserTurnVersioned is the cloud-mode start of a turn: read the stored history (it becomes the model's
// context in req.Messages) and append the question in the same compare-and-swap, so another replica's turn on the
// same thread lands next to this one instead of being replaced by it. The caller holds the conversation mutex. A
// history that cannot be read is neither used nor overwritten, as in the file path. stored says the question is in
// the store (written now, or already there under its id); write holds its versions, nil when the store has none.
// historyFailed says the history could not be read: the turn must not be answered without it (R-tool-loop-19).
func (s *ChatService) persistUserTurnVersioned(ctx context.Context, doc *models.FlowDocument, req *models.ChatRequest, convKey string) (stored bool, write *convWrite, historyFailed bool) {
	var history []models.ChatMessage
	loaded, already := false, false
	res, err := s.mutateConversationAt(ctx, doc.ID, convKey, func(h []models.ChatMessage) ([]models.ChatMessage, bool, error) {
		loaded, already = true, false
		history = h
		if req.UserMessage == "" {
			return h, false, nil
		}
		// Stored once: when its id is already there the question is done, and the model's context ends before it.
		if i := storedUserTurn(*req, h); i >= 0 {
			already, history = true, h[:i]
			return h, false, nil
		}
		return append(slices.Clone(h), newUserTurn(*req, h)), true, nil
	})
	if !loaded {
		logger.FromContext(ctx).Warn("chat: history load failed; the turn is not sent, and the stored conversation is left as it is",
			"error", err)
		req.Messages = nil
		return false, nil, true
	}
	req.Messages = history
	if err != nil {
		// The answer's write stores the question with it (persistAssistantTurn), so the thread keeps the pair.
		logger.FromContext(ctx).Warn("chat: failed to persist user turn at stream start; the answer's write will store it", "error", err)
	}
	if !res.Versioned {
		return res.Changed || already, nil, false
	}
	return res.Changed || already, &convWrite{Base: res.Loaded, Version: res.Version}, false
}

// ListThreads is the caller's thread index on a flow (R-frontend-c26 (c)): their stored conversations, newest change
// first, after the position after, at most limit. Unowned (pre-owner) conversations are in nobody's index. File
// storage (desktop, local modes) and a deployment without accounts keep no index: the list is empty.
func (s *ChatService) ListThreads(ctx context.Context, flowID string, after *storageif.PageCursor, limit int) ([]storageif.ConversationSummary, error) {
	owner := callerScope(ctx)
	if !s.useBackendConversations() || owner == "" {
		return []storageif.ConversationSummary{}, nil
	}
	return s.backend.ListConversations(ctx, flowID, owner, after, limit)
}

// SetThreadTitle names the caller's thread on a flow, creating its stored conversation empty when it has none yet
// (a thread named before its first turn); "" removes the name. The title is user text shown in every browser's index
// and in the export: stored clamped (ClampConversationTitle) and as its scrubbed copy, like the messages. Another
// user's key is 409 CONVERSATION_KEY_TAKEN, an unowned conversation 403 CONVERSATION_UNOWNED. The result is the
// conversation's version after the call (Versioned false where there are no versions). File storage and a
// deployment without accounts keep no index: accepted and dropped.
//
// A title stores something only where a turn would: with "Save conversation history" off it is accepted and dropped.
// It creates a conversation only under a thread id (storageif.IsThreadID), never under "flow" or another predictable
// key, which would take the key from every other user; a missing conversation under such a key is accepted and
// dropped. And it does not bring back a thread the caller cleared on this replica within clearedThreadTTL (a rename
// racing the close): the cleared thread is left gone, the title dropped.
func (s *ChatService) SetThreadTitle(ctx context.Context, flowID, key, title string) (ConversationSaved, error) {
	owner := callerScope(ctx)
	if !s.useBackendConversations() || owner == "" || !s.conversationHistoryEnabled(ctx) {
		return ConversationSaved{}, nil
	}
	// Under the thread's stripe lock, as a clear: a title and a clear of one thread on this replica run one after the
	// other, so the clear's mark is seen by any title that comes after its delete.
	mu := s.convMutexFor(flowID, key)
	if err := s.lockConversation(ctx, mu); err != nil {
		return ConversationSaved{}, err
	}
	defer mu.Unlock()
	if s.recentlyCleared(flowID, key, owner) {
		// Rename the thread only if it is stored again (a new turn after the clear); never recreate it.
		if _, v, err := s.backend.LoadConversationVersioned(ctx, flowID, key, owner); err != nil || v == 0 {
			return ConversationSaved{}, err
		}
	}
	title = ClampConversationTitle(historyScrubs.scrubText(owner, ClampConversationTitle(title)))
	version, err := s.backend.SetConversationTitle(ctx, flowID, key, owner, title)
	if errors.Is(err, storageif.ErrNotFound) {
		return ConversationSaved{}, nil
	}
	if err != nil {
		return ConversationSaved{}, conversationOwnerError(err)
	}
	return ConversationSaved{Version: version, Versioned: true}, nil
}

// clearedThreadTTL is how long a clear keeps SetThreadTitle from recreating the cleared thread: long past any rename
// in flight when the thread was closed. A test variable.
var clearedThreadTTL = 10 * time.Minute

// clearedThreadKey names one caller's thread on a flow in clearedThreads.
func clearedThreadKey(flowID, key, owner string) string {
	return flowID + "\x00" + key + "\x00" + owner
}

// markCleared records that owner cleared the thread, for recentlyCleared; the mark expires after clearedThreadTTL.
func (s *ChatService) markCleared(flowID, key, owner string) {
	k := clearedThreadKey(flowID, key, owner)
	at := time.Now()
	s.clearedThreads.Store(k, at)
	time.AfterFunc(clearedThreadTTL, func() { s.clearedThreads.CompareAndDelete(k, at) })
}

// recentlyCleared reports whether owner cleared the thread on this replica within clearedThreadTTL.
func (s *ChatService) recentlyCleared(flowID, key, owner string) bool {
	v, ok := s.clearedThreads.Load(clearedThreadKey(flowID, key, owner))
	return ok && time.Since(v.(time.Time)) < clearedThreadTTL
}
