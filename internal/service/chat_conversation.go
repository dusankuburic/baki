package service

import (
	"context"
	"encoding/json"
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
	return strings.TrimSpace(truncateRunes(label, storageif.AzureNameMaxLen))
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
		stored, err := s.backend.LoadConversation(ctx, flowID, provider)
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
		if err := s.backend.SaveConversation(ctx, flowID, provider, toStorageMessages(trimmed)); err != nil {
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

func (s *ChatService) ClearConversation(ctx context.Context, flowID, provider string) error {
	if flowID == "" {
		return fmt.Errorf("no flow loaded")
	}
	if s.useBackendConversations() {
		return s.backend.DeleteConversation(ctx, flowID, provider)
	}
	path, err := convFilePath(s.configDir, provider, flowID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete conversation file: %w", err)
	}
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
}

// persistAssistantTurn appends the finished answer server-side (a closed tab used to leave a stored
// question with no answer); best-effort, idempotent, bounded by persistAnswerTimeout. model/modelLabel persist the requested id and send-time label.
func (s *ChatService) persistAssistantTurn(ctx context.Context, doc *models.FlowDocument, req models.ChatRequest, ctl *streamCtl, model, modelLabel string) answerWrite {
	if doc == nil || ctl == nil {
		return answerWrite{}
	}
	ctl.mu.Lock()
	answer := ctl.buffer.String()
	done, errMsg := ctl.done, ctl.errMsg
	tokensIn, tokensOut := ctl.tokensIn, ctl.tokensOut
	served, inRate, outRate := ctl.servedModel, ctl.inputCostPerM, ctl.outputCostPerM
	events := ctl.snapshotEvents()
	ctl.mu.Unlock()
	if !done || errMsg != "" {
		return answerWrite{}
	}
	// The tool trail and fix records come from the stream's journal, as the
	// client pins them; a blank answer still stores when tools ran or a fix was proposed.
	toolCalls, fixes := journalTurnRecord(events)
	if strings.TrimSpace(answer) == "" && len(toolCalls) == 0 && len(fixes) == 0 {
		return answerWrite{}
	}

	convKey := req.ConversationKey()
	// The stream's context is cancelled as the worker unwinds; this write must
	// still land — within a bound (see persistAnswerTimeout).
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistAnswerTimeout)
	defer cancel()
	// The next turn may be reading the history this write replaces. The lock queues this replica's writers; the
	// compare-and-swap in mutateConversation settles the other replicas'.
	mu := s.convMutexFor(doc.ID, convKey)
	mu.Lock()
	defer mu.Unlock()
	answerID := clientMessageID(req.AssistantMessageID)
	// The answer as the store holds it, for the content match below: the client's save of the same answer holds
	// the write's scrubbed copy, and the buffer is only as masked as the stream scrubber left it.
	stored := historyScrubs.scrubText(callerScope(writeCtx), answer)
	// Usage, served model and billed rates as the client saved them on done,
	// so a reload keeps the tag and prices the turn at what it cost.
	msg := models.ChatMessage{
		ID:             answerID,
		Role:           "assistant",
		Content:        answer,
		Timestamp:      time.Now(),
		Provider:       req.Provider,
		Model:          model,
		ModelLabel:     modelLabel,
		TokensIn:       tokensIn,
		TokensOut:      tokensOut,
		ServedModel:    served,
		InputCostPerM:  inRate,
		OutputCostPerM: outRate,
		FinishReason:   "stop",
		ToolCalls:      toolCalls,
		FixProposals:   fixes,
	}
	res, err := s.mutateConversationAt(writeCtx, doc.ID, convKey, func(history []models.ChatMessage) ([]models.ChatMessage, bool, error) {
		// Already stored: a retry, or the client got there first; an id match
		// anywhere also covers its interrupted partial (else the reply shows twice).
		if slices.ContainsFunc(history, func(m models.ChatMessage) bool { return m.ID == answerID }) {
			return history, false, nil
		}
		if n := len(history); n > 0 && history[n-1].Role == "assistant" && history[n-1].Content == stored {
			return history, false, nil
		}
		// Right after its own question when stored under the client's id (a queued
		// follow-up's question can land in between) — else at the end.
		at := len(history)
		if id := req.UserMessageID; id != "" && clientMessageID(id) == id {
			if i := slices.IndexFunc(history, func(m models.ChatMessage) bool { return m.ID == id && m.Role == "user" }); i >= 0 {
				at = i + 1
			}
		}
		return slices.Insert(history, at, msg), true, nil
	})
	if err != nil {
		logger.Warn("chat: failed to store the finished answer", "error", err)
		return answerWrite{}
	}
	return answerWrite{Written: res.Changed && res.Versioned, Loaded: res.Loaded, Version: res.Version}
}

// Card and item statuses the stored fix records may carry — the client's own
// vocabulary (chatStore), to which fix_decision statuses are clamped.
var (
	fixCardStatuses = map[string]bool{"applying": true, "applied": true, "applied-unresolved": true, "declined": true, "timeout": true, "cancelled": true, "error": true}
	fixItemStatuses = map[string]bool{"applied": true, "applied-unresolved": true, "error": true, "already-resolved": true}
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
