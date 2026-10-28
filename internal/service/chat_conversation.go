package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/storage/atomicfile"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// ClampModelLabel clamps a client-sent ModelLabel for storage: NFC, control/bidi/zero-width runes dropped,
// whitespace collapsed, 64 chars max — clamped (a refusal fails the save); it removes what impersonates another model.
func ClampModelLabel(label string) string {
	return clampLabel(label, storageif.AzureNameMaxLen)
}

// ClampConversationTitle clamps a thread title for storage as ClampModelLabel does a label, to
// storageif.ConversationTitleMaxRunes (R-frontend-c26 (c)).
func ClampConversationTitle(title string) string {
	return clampLabel(title, storageif.ConversationTitleMaxRunes)
}

// clampLabel is NFC, control/bidi/zero-width runes dropped, whitespace collapsed, at most maxRunes runes.
func clampLabel(label string, maxRunes int) string {
	if label == "" {
		return ""
	}
	label = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.In(r, unicode.Cc, unicode.Cf):
			return -1
		}
		return r
	}, strings.ToValidUTF8(label, ""))
	label = strings.Join(strings.Fields(norm.NFC.String(label)), " ")
	return strings.TrimSpace(truncateRunes(label, maxRunes))
}

// reconstructHistory loads the stored conversation for the request's key so a
// client that sent only the new userMessage still gets full LLM context.
func (s *ChatService) reconstructHistory(ctx context.Context, doc *models.FlowDocument, req models.ChatRequest) ([]models.ChatMessage, error) {
	if doc == nil {
		return nil, nil
	}
	loaded, err := s.GetConversation(ctx, doc.ID, req.ConversationKey())
	if err != nil {
		// The error is RETURNED, not swallowed: SaveConversation is a full
		// replace, so persisting on an unreadable conversation is data loss.
		return nil, err
	}
	return loaded, nil
}

// clientMessageID returns the client-supplied id when it is a canonical UUID,
// else a fresh one — sharing the id lets a mid-turn reload dedupe the copy.
func clientMessageID(id string) string {
	// Canonical 8-4-4-4-12 form only: uuid.Parse also accepts braced, urn: and
	// 32-hex spellings, which the client's own id comparison would not match.
	if len(id) == 36 {
		if u, err := uuid.Parse(id); err == nil && strings.EqualFold(u.String(), id) {
			return id
		}
	}
	return uuid.NewString()
}

// useBackendConversations reports whether conversations persist through the backend (cloud: durable,
// cross-replica, RLS) rather than local files — allow-listed on ModeCloud; any other mode falls back, never Postgres.
func (s *ChatService) useBackendConversations() bool {
	return s.mode == config.ModeCloud && s.backend != nil
}

func (s *ChatService) GetConversation(ctx context.Context, flowID, provider string) ([]models.ChatMessage, error) {
	if flowID == "" {
		return nil, fmt.Errorf("no flow loaded")
	}
	if s.useBackendConversations() {
		// The caller's view (R-security-3): another user's conversation under the key reads as none.
		stored, _, err := s.backend.LoadConversationVersioned(ctx, flowID, provider, callerScope(ctx))
		if err != nil {
			return nil, err
		}
		return toModelMessages(stored), nil
	}
	path, err := convFilePath(s.configDir, provider, flowID)
	if err != nil {
		return nil, err
	}
	// Tolerant: on Windows a read racing a save's rename fails transiently (a 500 to a polling reload).
	data, err := atomicfile.ReadFileTolerant(path) // #nosec G304 -- path derived from configDir+provider+flowID, not raw user input
	if err != nil {
		if os.IsNotExist(err) {
			return []models.ChatMessage{}, nil
		}
		return nil, fmt.Errorf("failed to read conversation file: %w", err)
	}

	var conv models.ConversationFile
	if err := json.Unmarshal(data, &conv); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation: %w", err)
	}
	return conv.Messages, nil
}

func (s *ChatService) SaveConversation(ctx context.Context, flowID, provider string, messages []models.ChatMessage) error {
	_, err := s.saveConversation(ctx, flowID, provider, messages)
	return err
}

// normalizeConversation is what a history is trimmed to before it is stored: blank assistant messages dropped,
// every content stored as its scrubbed copy, the list evicted to the caps. Every write path runs it.
//
// The store holds the copy the provider sees: a pasted connection string, Bearer token or key is masked before it
// reaches the row or the file, so a reload, a viewer holding the thread's key and the data-subject export get the
// masked text. The scrub is the history memo's (historyScrubs, keyed by scope and content), the one the turn's
// request build uses: a write runs the secret regexes over the whole list, twice a turn and under the thread's
// mutex before the first token, and unmemoised that is ~0.4 s per write on an anchor-bearing history near the cap,
// for the life of the thread. With the memo a stored copy is scrubbed once per process, and the question's write
// warms the entry the request build reads. ScrubFreeText is idempotent on its own output, which keeps the writers'
// no-change detection (the mutation's changed flag, the answer dedupe in persistAssistantTurn) exact across
// re-stores.
func normalizeConversation(scope string, messages []models.ChatMessage) []models.ChatMessage {
	// Drop a blank assistant message: an empty 'stop' answer used to brick the
	// thread on Claude (Messages API rejects {"role":"assistant","content":""}).
	filtered := make([]models.ChatMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" &&
			len(m.ToolCalls) == 0 && len(m.FixProposals) == 0 && m.FixProposal == nil {
			continue
		}
		// On the copy: the caller's list (a request's messages) is left as it came.
		m.Content = historyScrubs.scrubText(scope, m.Content)
		filtered = append(filtered, m)
	}
	return evictConvMessages(filtered)
}

// callerScope is the scope the store's writes scrub under: the caller as the handlers' KeyScope names it (the
// request's claims, or none when accounts are off), so a write's memo entries are the ones the turn's request build
// (buildScopedMessages) reads under the same scope. From the context because the writes at stream start and stream
// end are reached without the scope; the worker's context keeps the request's claims.
func callerScope(ctx context.Context) string {
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		return claims.UserID
	}
	return ""
}

// saveConversation is SaveConversation reporting whether the list was stored
// (false/nil = "Save conversation history" off: accepted and dropped).
func (s *ChatService) saveConversation(ctx context.Context, flowID, provider string, messages []models.ChatMessage) (stored bool, err error) {
	if flowID == "" {
		return false, fmt.Errorf("no flow loaded")
	}
	// "Save conversation history" off: accept and drop (covers the server's
	// own start-of-stream persist and older clients that kept calling).
	if !s.conversationHistoryEnabled(ctx) {
		return false, nil
	}
	trimmed := normalizeConversation(callerScope(ctx), messages)

	if s.useBackendConversations() {
		if err := s.replaceConversation(ctx, flowID, provider, toStorageMessages(trimmed)); err != nil {
			return false, err
		}
		return true, nil
	}

	path, err := convFilePath(s.configDir, provider, flowID)
	if err != nil {
		return false, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return false, fmt.Errorf("failed to create conversations directory: %w", err)
	}

	conv := models.ConversationFile{
		Version:   1,
		FlowKey:   flowID,
		Scope:     provider,
		UpdatedAt: time.Now(),
		Messages:  trimmed,
	}

	data, err := json.MarshalIndent(conv, "", "  ")
	if err != nil {
		return false, fmt.Errorf("failed to marshal conversation: %w", err)
	}

	// Atomic write at 0600: an interrupted write leaves the prior conversation
	// intact rather than truncating it, and the file isn't world-readable.
	if err := atomicWriteConv(dir, path, data); err != nil {
		return false, err
	}
	return true, nil
}

// replaceConversation is the blind full replace of the caller's conversation in the store (a client save without a
// base version): a compare-and-swap at whatever version it reads, again while other writers win, so it replaces
// only the caller's own (or an unowned) conversation (R-security-3); another user's under the key is
// ErrConversationKeyTaken.
func (s *ChatService) replaceConversation(ctx context.Context, flowID, key string, messages []storageif.ChatMessage) error {
	owner := callerScope(ctx)
	for attempt := 1; ; attempt++ {
		_, version, err := s.backend.LoadConversationVersioned(ctx, flowID, key, owner)
		if err != nil {
			return err
		}
		_, err = s.backend.SaveConversationIf(ctx, flowID, key, owner, version, messages)
		if !errors.Is(err, storageif.ErrConversationConflict) || attempt >= mutateConversationAttempts {
			return err
		}
	}
}

// ClearConversation deletes the caller's conversation whatever its version (a client clear without a base version).
// In the store that is a delete at the version it reads, again while other writers win: the caller's own
// conversation only (another user's under the key is a missing one), an unowned one only with mayDeleteUnowned, else
// 403 CONVERSATION_UNOWNED (R-security-3). Files have one user and no owners.
func (s *ChatService) ClearConversation(ctx context.Context, flowID, provider string, mayDeleteUnowned bool) error {
	if flowID == "" {
		return fmt.Errorf("no flow loaded")
	}
	err := s.clearingConversation(ctx, flowID, provider, func() error {
		if s.useBackendConversations() {
			owner := callerScope(ctx)
			for attempt := 1; ; attempt++ {
				_, version, err := s.backend.LoadConversationVersioned(ctx, flowID, provider, owner)
				if err != nil {
					return err
				}
				err = s.backend.DeleteConversationIf(ctx, flowID, provider, owner, version, mayDeleteUnowned)
				if !errors.Is(err, storageif.ErrConversationConflict) || attempt >= mutateConversationAttempts {
					return err
				}
			}
		}
		path, err := convFilePath(s.configDir, provider, flowID)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to delete conversation file: %w", err)
		}
		return nil
	})
	return conversationOwnerError(err)
}

// clearingConversation runs a conversation's delete under its stripe lock and, when it succeeds, tells this
// replica's streams on the conversation not to write it again: the stopped turn of a thread closed mid-stream would
// otherwise bring the deleted conversation back (R-frontend-c26). An answer write holds the same lock and checks the
// mark after taking it, so it lands either before the delete (and is deleted) or not at all. Only the caller's own
// streams are marked (all of them without accounts): a clear deletes only the caller's conversation, so another
// user's stream on the same key is writing their own (R-security-3).
func (s *ChatService) clearingConversation(ctx context.Context, flowID, convKey string, del func() error) error {
	mu := s.convMutexFor(flowID, convKey)
	if err := s.lockConversation(ctx, mu); err != nil {
		return err
	}
	defer mu.Unlock()
	if err := del(); err != nil {
		return err
	}
	caller := callerScope(ctx)
	if caller != "" {
		s.markCleared(flowID, convKey, caller)
	}
	mark := func(_, v interface{}) bool {
		if c, ok := v.(*streamCtl); ok && c.flowID == flowID && c.convKey == convKey && (caller == "" || c.ownerID == caller) {
			c.conversationCleared.Store(true)
		}
		return true
	}
	s.activeStreams.Range(mark)
	s.finishedStreams.Range(mark)
	return nil
}

// persistAnswerTimeout bounds the detached end-of-stream write: unbounded, a
// stalled store parked every finishing stream on the stripe mutex. Test variable.
var persistAnswerTimeout = 15 * time.Second

// answerWrite is what persistAssistantTurn did: Written says this call put the answer in the store, and the versions
// say what the conversation looked like around that write (see mutation); the zero value is "nothing written".
type answerWrite struct {
	Written         bool
	Loaded, Version int64
	// Failed: the write was due and failed after its retries (counted in pad_chat_answer_write_failures_total).
	Failed bool
}

// answerWriteAttempts bounds the retries of an answer write that failed for a reason other than a lost round (a
// database hiccup); all of them stay within persistAnswerTimeout.
const answerWriteAttempts = 3

// answerWriteRetryDelay is the pause before retry n of an answer write (times n). A test variable.
var answerWriteRetryDelay = 200 * time.Millisecond

// persistAssistantTurn appends the finished answer server-side (a closed tab used to leave a stored
// question with no answer); best-effort, idempotent, bounded by persistAnswerTimeout. model/modelLabel persist the requested id and send-time label.
//
// A turn that ended without an answer is stored too when it left something behind (R-approvals-4): a fix card (the
// flow may have changed), a tool trail, or partial text — as the client shows it, finish reason "error" with the
// code and the error line, or "interrupted" for a stopped one. A turn the server's stop ended is stored only for
// its fix cards: the client re-sends it.
func (s *ChatService) persistAssistantTurn(ctx context.Context, doc *models.FlowDocument, req models.ChatRequest, ctl *streamCtl, model, modelLabel string) answerWrite {
	if doc == nil || ctl == nil {
		return answerWrite{}
	}
	ctl.mu.Lock()
	answer := ctl.buffer.String()
	done, errMsg, errCode, errShared := ctl.done, ctl.errMsg, ctl.errCode, ctl.errShared
	tokensIn, tokensOut := ctl.tokensIn, ctl.tokensOut
	served := ctl.servedModel
	events := ctl.snapshotEvents()
	truncated := ctl.droppedHistory
	knowledge := ctl.knowledgeStatus
	ctl.mu.Unlock()
	failed := errMsg != ""
	if !done && !failed {
		return answerWrite{}
	}
	// The tool trail and fix records come from the stream's journal, as the
	// client pins them; a blank answer still stores when tools ran or a fix was proposed.
	toolCalls, fixes := journalTurnRecord(events)
	if strings.TrimSpace(answer) == "" && len(toolCalls) == 0 && len(fixes) == 0 {
		return answerWrite{}
	}
	if failed && errCode == ChatErrServerRestarting && len(fixes) == 0 {
		return answerWrite{}
	}
	finish, content := "stop", answer
	if failed {
		finish, content = failedTurnRecord(answer, errCode, errMsg)
	}

	convKey := req.ConversationKey()
	// The stream's context is cancelled as the worker unwinds; this write must
	// still land — within a bound (see persistAnswerTimeout).
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistAnswerTimeout)
	defer cancel()
	// The server's stop, once out of budget, cuts the write: the done (answerStored false, the client saves the
	// turn) must leave before the connections close (R-streaming-c16).
	watched := make(chan struct{})
	defer close(watched)
	go func() {
		select {
		case <-s.stopped():
			cancel()
		case <-watched:
		}
	}()
	// The next turn may be reading the history this write replaces. The lock queues this replica's writers; the
	// compare-and-swap in mutateConversation settles the other replicas'.
	mu := s.convMutexFor(doc.ID, convKey)
	if err := s.lockConversation(writeCtx, mu); err != nil {
		chatAnswerWriteFailuresTotal.WithLabelValues("timeout").Inc()
		logger.FromContext(ctx).Warn("chat: gave up waiting to store the finished answer; the client is told to save it", "error", err,
			"conversation", conversationLogID(doc.ID, convKey))
		return answerWrite{Failed: true}
	}
	defer mu.Unlock()
	if ctl.conversationCleared.Load() {
		// The thread was deleted while this stream ran (closed, or evicted): writing now would bring it back, with
		// no client left to show it.
		return answerWrite{}
	}
	answerID := clientMessageID(req.AssistantMessageID)
	// The answer as the store holds it, for the content match below: the client's save of the same answer holds
	// the write's scrubbed copy, and the buffer is only as masked as the stream scrubber left it.
	stored := historyScrubs.scrubText(callerScope(writeCtx), answer)
	// Usage, served model and billed rates as the client saved them on done,
	// so a reload keeps the tag and prices the turn at what it cost.
	msg := models.ChatMessage{
		ID:           answerID,
		Role:         "assistant",
		Content:      content,
		Timestamp:    time.Now(),
		Provider:     req.Provider,
		Model:        model,
		ModelLabel:   modelLabel,
		TokensIn:     tokensIn,
		TokensOut:    tokensOut,
		ServedModel:  served,
		FinishReason: finish,
		ToolCalls:    toolCalls,
		FixProposals: fixes,
		Truncated:    truncated,
		Knowledge:    knowledge,
	}
	if failed && finish == "error" {
		msg.ErrorCode, msg.ErrorShared = errCode, errShared
	}
	// pairWritten: the last round stored the question with the answer (its write at the start had failed).
	pairWritten := false
	mutate := func(history []models.ChatMessage) ([]models.ChatMessage, bool, error) {
		pairWritten = false // per compare-and-swap round: only the round that lands counts
		// Already stored: a retry, or the client got there first; an id match
		// anywhere also covers its interrupted partial (else the reply shows twice).
		// The client's copy may hold a card it stored mid-apply (a stop, a closed thread): the journal knows the outcome,
		// and the card takes it (R-approvals-5).
		if i := slices.IndexFunc(history, func(m models.ChatMessage) bool { return m.ID == answerID }); i >= 0 {
			return settleStoredCards(history, i, fixes)
		}
		if n := len(history); n > 0 && history[n-1].Role == "assistant" && history[n-1].Content == stored {
			return settleStoredCards(history, n-1, fixes)
		}
		// Right after its own question when stored under the client's id (a queued
		// follow-up's question can land in between) — else at the end.
		at := len(history)
		if id := req.UserMessageID; id != "" && clientMessageID(id) == id {
			if i := slices.IndexFunc(history, func(m models.ChatMessage) bool { return m.ID == id && m.Role == "user" }); i >= 0 {
				at = i + 1
			} else if req.UserMessage != "" && !ctl.userTurnStored.Load() {
				// The question's write failed at the start (every round lost, or the store failed): it is stored
				// here with its answer, so the thread holds both or neither — never an answer with no question
				// (R-replicas-12).
				pairWritten = true
				return append(slices.Clone(history), newUserTurn(req, history), msg), true, nil
			}
		}
		return slices.Insert(history, at, msg), true, nil
	}
	var res mutation
	var err error
	for attempt := 1; ; attempt++ {
		pairWritten = false
		res, err = s.mutateConversationAt(writeCtx, doc.ID, convKey, mutate)
		// A key that holds another user's conversation (R-security-3) refuses every retry as well.
		if err == nil || errors.Is(err, storageif.ErrConversationConflict) || errors.Is(err, storageif.ErrConversationKeyTaken) ||
			writeCtx.Err() != nil || attempt >= answerWriteAttempts {
			break
		}
		select {
		case <-time.After(time.Duration(attempt) * answerWriteRetryDelay):
		case <-writeCtx.Done():
		}
	}
	if err != nil {
		cause := "error"
		switch {
		case errors.Is(err, storageif.ErrConversationConflict):
			cause = "conflict"
		case writeCtx.Err() != nil:
			cause = "timeout"
		}
		chatAnswerWriteFailuresTotal.WithLabelValues(cause).Inc()
		logger.FromContext(ctx).Warn("chat: failed to store the finished answer; the client is told to save it", "cause", cause, "error", err,
			"conversation", conversationLogID(doc.ID, convKey))
		return answerWrite{Failed: true}
	}
	if pairWritten && res.Changed {
		chatConversationConflictsTotal.WithLabelValues("answer_with_question").Inc()
		ctl.userTurnStored.Store(true)
		logger.FromContext(ctx).Warn("chat: stored a turn's question with its answer: the question's write at the start had failed",
			"source", "answer_with_question", "conversation", conversationLogID(doc.ID, convKey))
	}
	return answerWrite{Written: res.Changed && res.Versioned, Loaded: res.Loaded, Version: res.Version}
}

// settleStoredCards gives the cards of history[i] that are still undecided (pending, applying) the terminal outcome the
// journal recorded for them; nothing else of the message changes. The history is copied only when a card changed.
func settleStoredCards(history []models.ChatMessage, i int, journal []models.FixProposalSnapshot) ([]models.ChatMessage, bool, error) {
	open := func(status string) bool { return status == "pending" || status == "applying" }
	byID := make(map[string]models.FixProposalSnapshot, len(journal))
	for _, fp := range journal {
		if !open(fp.Status) {
			byID[fp.ProposalID] = fp
		}
	}
	msg := history[i]
	var cards []models.FixProposalSnapshot
	for j, fp := range msg.FixProposals {
		done, ok := byID[fp.ProposalID]
		if !ok || !open(fp.Status) {
			continue
		}
		if cards == nil {
			cards = slices.Clone(msg.FixProposals)
		}
		fp.Status, fp.Message = done.Status, done.Message
		fp.Items = slices.Clone(fp.Items)
		for k := range fp.Items {
			for _, it := range done.Items {
				same := it.ID != "" && fp.Items[k].ID == it.ID || it.ID == "" && fp.Items[k].RuleID == it.RuleID
				if same && open(fp.Items[k].Status) {
					fp.Items[k].Status, fp.Items[k].Message = it.Status, it.Message
				}
			}
		}
		cards[j] = fp
	}
	if cards == nil {
		return history, false, nil
	}
	out := slices.Clone(history)
	msg.FixProposals = cards
	out[i] = msg
	return out, true, nil
}

// failedTurnRecord is a failed turn's stored finish reason and content, in the shape the client gives the message: a
// stopped turn is "interrupted" with its text as it was; any other failure is "error", the text followed by the
// error line (the client's chat:errors.generic).
func failedTurnRecord(answer, code, msg string) (finish, content string) {
	if code == ChatErrStopped {
		return "interrupted", answer
	}
	line := failedTurnLinePrefix + msg + "*"
	if strings.TrimSpace(answer) == "" {
		return "error", line
	}
	return "error", answer + failedTurnSeparator + line
}

// The error line a failed turn's stored content ends with (failedTurnRecord), after the separator when text streamed.
const (
	failedTurnSeparator  = "\n\n---\n"
	failedTurnLinePrefix = "*Error: "
)

// historyContent is what the model reads of a stored message: a failed turn's partial text without its error line
// (the report of the failure, not something the assistant said); "" when the turn left no text. The client and the
// server word that line differently (a provider's title, the restart copy), but both put it after the last
// separator, and a failed message without one is the error line alone.
func historyContent(m models.ChatMessage) string {
	if m.FinishReason != "error" {
		return m.Content
	}
	if i := strings.LastIndex(m.Content, failedTurnSeparator); i >= 0 {
		return m.Content[:i]
	}
	return ""
}

// Card and item statuses the stored fix records may carry — the client's own
// vocabulary (chatStore), to which fix_decision statuses are clamped.
var (
	fixCardStatuses = map[string]bool{"applying": true, "applied": true, "applied-unresolved": true, "already-resolved": true, "declined": true, "timeout": true, "cancelled": true, "error": true}
	fixItemStatuses = map[string]bool{"applied": true, "applied-unresolved": true, "error": true, "already-resolved": true, "skipped": true}
)

// journalTurnRecord derives the turn's tool trail and fix records from the
// journal (tool_result → ToolCallRecords; proposals resolve by decision events).
func journalTurnRecord(events []journalEvent) (calls []models.ToolCallRecord, fixes []models.FixProposalSnapshot) {
	byID := make(map[string]int)
	for _, ev := range events {
		switch ev.Type {
		case "tool_result":
			calls = append(calls, models.ToolCallRecord{
				Name:       journalString(ev.Data["name"]),
				Label:      journalString(ev.Data["label"]),
				Ok:         ev.Data["ok"] == true,
				DurationMs: journalInt(ev.Data["durationMs"]),
				Summary:    journalString(ev.Data["summary"]),
			})
		case "fix_proposal":
			id := journalString(ev.Data["proposalId"])
			if _, dup := byID[id]; id == "" || dup {
				continue
			}
			var items []models.FixItemSnapshot
			for _, raw := range journalItems(ev.Data["items"]) {
				items = append(items, models.FixItemSnapshot{
					ID:         journalString(raw["id"]),
					RuleID:     journalString(raw["ruleId"]),
					FixType:    journalString(raw["fixType"]),
					BlockLabel: journalString(raw["blockLabel"]),
					Line:       int(journalInt(raw["line"])),
					Summary:    journalString(raw["summary"]),
					Status:     "pending",
				})
			}
			if len(items) == 0 { // a single fix: flat fields, one item
				items = []models.FixItemSnapshot{{
					RuleID:     journalString(ev.Data["ruleId"]),
					FixType:    journalString(ev.Data["fixType"]),
					BlockLabel: journalString(ev.Data["blockLabel"]),
					Line:       int(journalInt(ev.Data["line"])),
					Summary:    journalString(ev.Data["summary"]),
					Status:     "pending",
				}}
			}
			byID[id] = len(fixes)
			fixes = append(fixes, models.FixProposalSnapshot{
				ProposalID:  id,
				Tool:        journalString(ev.Data["tool"]),
				PreviewKind: journalString(ev.Data["previewKind"]),
				RuleID:      items[0].RuleID,
				FixType:     items[0].FixType,
				BlockLabel:  items[0].BlockLabel,
				Line:        items[0].Line,
				Summary:     items[0].Summary,
				Status:      "pending",
				Items:       items,
			})
		case "fix_decision":
			idx, ok := byID[journalString(ev.Data["proposalId"])]
			if !ok {
				continue
			}
			snap := &fixes[idx]
			status := journalString(ev.Data["status"])
			if !fixCardStatuses[status] {
				status = "error"
			}
			snap.Status = status
			snap.Message = journalString(ev.Data["message"])
			decided := journalItems(ev.Data["items"])
			for _, raw := range decided {
				// By id when both sides carry one (two fixes of one rule); by ruleId for records before it.
				id, rid := journalString(raw["id"]), journalString(raw["ruleId"])
				for i := range snap.Items {
					if id != "" && snap.Items[i].ID != "" {
						if snap.Items[i].ID != id {
							continue
						}
					} else if snap.Items[i].RuleID != rid {
						continue
					}
					snap.Items[i].Status = journalString(raw["status"])
					if !fixItemStatuses[snap.Items[i].Status] {
						snap.Items[i].Status = "error"
					}
					snap.Items[i].Message = journalString(raw["message"])
				}
			}
			// A single fix's decision carries no items: its one item takes the
			// terminal outcome; "applying" is not terminal and stays pending.
			if len(decided) == 0 && len(snap.Items) == 1 && status != "applying" {
				item := status
				if !fixItemStatuses[item] {
					item = "error"
				}
				snap.Items[0].Status = item
				snap.Items[0].Message = snap.Message
			}
		}
	}
	return calls, fixes
}

// journalItems reads a journal event's items list — the []map the stream
// emits, or the []interface{} a JSON round trip leaves.
func journalItems(v interface{}) []map[string]interface{} {
	switch items := v.(type) {
	case []map[string]interface{}:
		return items
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(items))
		for _, it := range items {
			if m, ok := it.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

func journalString(v interface{}) string {
	s, _ := v.(string)
	return s
}

// journalInt reads a journal number as the stream emits it (int / int64) or
// as a JSON round trip leaves it (float64).
func journalInt(v interface{}) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}
