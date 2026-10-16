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

// mutateConversationAttempts bounds a writer's read-modify-write rounds. A round is lost only to another writer that
// finished its own first, so each lost round is someone else's progress; five covers a handful of replicas writing
// one thread at once, and the per-conversation lock keeps the contenders of one replica to a single at a time.
const mutateConversationAttempts = 5

// mutateConversationBackoff is the pause before round attempt+1: short and jittered so replicas that lost together
// do not meet again. A test variable.
var mutateConversationBackoff = func(int) time.Duration {
	return 10*time.Millisecond + rand.N(40*time.Millisecond) // #nosec G404 -- jitter only, not security-sensitive
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

// conversationConflictSources is the closed value set of the source label.
var conversationConflictSources = []string{"client_save", "client_clear", "retry", "retry_exhausted"}

func init() {
	metrics.MustRegister(chatBlindSavesTotal, chatPersistGiveUpsTotal, chatConversationConflictsTotal)
	// Every source is exported from the first scrape, so a rate over it starts at zero instead of absent.
	for _, src := range conversationConflictSources {
		chatConversationConflictsTotal.WithLabelValues(src)
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
		stored, version, err := s.backend.LoadConversationVersioned(ctx, flowID, key)
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
		written, err := s.backend.SaveConversationIf(ctx, flowID, key, version, toStorageMessages(normalizeConversation(out)))
		if err == nil {
			return mutation{Changed: true, Loaded: version, Version: written, Versioned: true}, nil
		}
		if !errors.Is(err, storageif.ErrConversationConflict) {
			return mutation{}, err
		}
		if attempt >= mutateConversationAttempts {
			chatPersistGiveUpsTotal.Inc()
			chatConversationConflictsTotal.WithLabelValues("retry_exhausted").Inc()
			logger.Warn("chat: gave up a conversation write after losing every round to concurrent writers",
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
func (s *ChatService) SaveClientConversation(ctx context.Context, flowID, provider string, base *int64, messages []models.ChatMessage) (ConversationSaved, error) {
	if flowID == "" {
		return ConversationSaved{}, fmt.Errorf("no flow loaded")
	}
	if !s.useBackendConversations() {
		_, err := s.saveConversation(ctx, flowID, provider, messages)
		return ConversationSaved{}, err
	}
	if base == nil {
		chatBlindSavesTotal.Inc()
		_, err := s.saveConversation(ctx, flowID, provider, messages)
		return ConversationSaved{}, err
	}
	// "Save conversation history" off: accepted and dropped, the version unchanged.
	if !s.conversationHistoryEnabled(ctx) {
		return ConversationSaved{Version: *base, Versioned: true}, nil
	}
	version, err := s.backend.SaveConversationIf(ctx, flowID, provider, *base, toStorageMessages(normalizeConversation(messages)))
	if errors.Is(err, storageif.ErrConversationConflict) {
		chatConversationConflictsTotal.WithLabelValues("client_save").Inc()
		return ConversationSaved{}, conversationConflict(err, version)
	}
	if err != nil {
		return ConversationSaved{}, err
	}
	return ConversationSaved{Version: version, Versioned: true}, nil
}

// ClearConversationIf deletes the conversation only while it is at the version the client loaded; a changed one is
// the same 409 CONVERSATION_CONFLICT as a stale save. File storage has no versions and clears as ClearConversation.
func (s *ChatService) ClearConversationIf(ctx context.Context, flowID, provider string, base int64) error {
	if flowID == "" {
		return fmt.Errorf("no flow loaded")
	}
	if !s.useBackendConversations() {
		return s.ClearConversation(ctx, flowID, provider)
	}
	err := s.backend.DeleteConversationIf(ctx, flowID, provider, base)
	if !errors.Is(err, storageif.ErrConversationConflict) {
		return err
	}
	chatConversationConflictsTotal.WithLabelValues("client_clear").Inc()
	// Only a hint for the client: when the read fails it learns the version by reloading.
	_, current, lerr := s.backend.LoadConversationVersioned(ctx, flowID, provider)
	if lerr != nil {
		logger.Debug("chat: could not read the current conversation version for a conflict", "error", lerr)
	}
	return conversationConflict(err, current)
}

// conversationConflict is the API error of a lost compare-and-swap.
func conversationConflict(cause error, current int64) error {
	return apperr.Conflict(CodeConversationConflict, "this conversation changed since you loaded it; reload it and try again").
		WithDetails(map[string]any{"version": current}).
		Wrap(cause)
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
		if stored, out.Version, err = s.backend.LoadConversationVersioned(ctx, flowID, convKey); err != nil {
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
		ID:             userID,
		Role:           "user",
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
func (s *ChatService) persistUserTurnVersioned(ctx context.Context, doc *models.FlowDocument, req *models.ChatRequest, convKey string) (stored bool, write *convWrite) {
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
		logger.Warn("chat: history load failed; answering without prior context and "+
			"skipping persistence so the stored conversation is not overwritten", "error", err)
		req.Messages = nil
		return false, nil
	}
	req.Messages = history
	if err != nil {
		logger.Warn("chat: failed to persist user turn at stream start", "error", err)
	}
	if !res.Versioned {
		return res.Changed || already, nil
	}
	return res.Changed || already, &convWrite{Base: res.Loaded, Version: res.Version}
}
