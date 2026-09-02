package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// toStorageMessages converts domain chat messages to the storage-layer type for
// backend (cloud) persistence. It is the lossless inverse of toModelMessages;
// the storage type carries Timestamp as an RFC3339Nano string (matching how
// models.ChatMessage's time.Time marshals), so each message encodes identically
// on both paths. (The filesystem path additionally wraps the message array in a
// Conversation{FlowKey,Scope,UpdatedAt} envelope; the backend stores the bare
// array. The per-message parity is what keeps the bridge lossless.)
func toStorageMessages(msgs []models.ChatMessage) []storageif.ChatMessage {
	out := make([]storageif.ChatMessage, len(msgs))
	for i, m := range msgs {
		ts := ""
		if !m.Timestamp.IsZero() {
			ts = m.Timestamp.UTC().Format(time.RFC3339Nano)
		}
		out[i] = storageif.ChatMessage{
			ID:               m.ID,
			Role:             m.Role,
			Content:          m.Content,
			Timestamp:        ts,
			ContextBlockID:   m.ContextBlockID,
			ContextSubflowID: m.ContextSubflowID,
			TokensIn:         m.TokensIn,
			TokensOut:        m.TokensOut,
			Provider:         m.Provider,
			Model:            m.Model,
			FinishReason:     m.FinishReason,
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]storageif.ToolCallRecord, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				calls[j] = storageif.ToolCallRecord{
					Name:       tc.Name,
					Label:      tc.Label,
					Ok:         tc.Ok,
					DurationMs: tc.DurationMs,
					Summary:    tc.Summary,
				}
			}
			out[i].ToolCalls = calls
		}
		if m.FixProposal != nil {
			out[i].FixProposal = &storageif.FixProposalSnapshot{
				ProposalID: m.FixProposal.ProposalID,
				RuleID:     m.FixProposal.RuleID,
				FixType:    m.FixProposal.FixType,
				BlockLabel: m.FixProposal.BlockLabel,
				Line:       m.FixProposal.Line,
				Summary:    m.FixProposal.Summary,
				Status:     m.FixProposal.Status,
				Message:    m.FixProposal.Message,
			}
			out[i].FixProposal.Items = convertItemsToStorage(m.FixProposal.Items)
		}
		if len(m.FixProposals) > 0 {
			proposals := make([]storageif.FixProposalSnapshot, len(m.FixProposals))
			for j, fp := range m.FixProposals {
				proposals[j] = storageif.FixProposalSnapshot{
					ProposalID: fp.ProposalID,
					RuleID:     fp.RuleID,
					FixType:    fp.FixType,
					BlockLabel: fp.BlockLabel,
					Line:       fp.Line,
					Summary:    fp.Summary,
					Status:     fp.Status,
					Message:    fp.Message,
				}
				proposals[j].Items = convertItemsToStorage(fp.Items)
			}
			out[i].FixProposals = proposals
		}
	}
	return out
}

func convertItemsToStorage(items []models.FixItemSnapshot) []storageif.FixItemSnapshot {
	if len(items) == 0 {
		return nil
	}
	out := make([]storageif.FixItemSnapshot, len(items))
	for k, it := range items {
		out[k] = storageif.FixItemSnapshot{
			RuleID:     it.RuleID,
			FixType:    it.FixType,
			BlockLabel: it.BlockLabel,
			Line:       it.Line,
			Summary:    it.Summary,
			Status:     it.Status,
			Message:    it.Message,
		}
	}
	return out
}

func convertItemsToModel(items []storageif.FixItemSnapshot) []models.FixItemSnapshot {
	if len(items) == 0 {
		return nil
	}
	out := make([]models.FixItemSnapshot, len(items))
	for k, it := range items {
		out[k] = models.FixItemSnapshot{
			RuleID:     it.RuleID,
			FixType:    it.FixType,
			BlockLabel: it.BlockLabel,
			Line:       it.Line,
			Summary:    it.Summary,
			Status:     it.Status,
			Message:    it.Message,
		}
	}
	return out
}

// toModelMessages is the inverse of toStorageMessages. A blank or unparseable
// timestamp decodes to the zero time rather than failing the whole load.
func toModelMessages(msgs []storageif.ChatMessage) []models.ChatMessage {
	out := make([]models.ChatMessage, len(msgs))
	for i, m := range msgs {
		var ts time.Time
		if m.Timestamp != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, m.Timestamp); err == nil {
				ts = parsed
			}
		}
		out[i] = models.ChatMessage{
			ID:               m.ID,
			Role:             m.Role,
			Content:          m.Content,
			Timestamp:        ts,
			ContextBlockID:   m.ContextBlockID,
			ContextSubflowID: m.ContextSubflowID,
			TokensIn:         m.TokensIn,
			TokensOut:        m.TokensOut,
			Provider:         m.Provider,
			Model:            m.Model,
			FinishReason:     m.FinishReason,
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]models.ToolCallRecord, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				calls[j] = models.ToolCallRecord{
					Name:       tc.Name,
					Label:      tc.Label,
					Ok:         tc.Ok,
					DurationMs: tc.DurationMs,
					Summary:    tc.Summary,
				}
			}
			out[i].ToolCalls = calls
		}
		if m.FixProposal != nil {
			out[i].FixProposal = &models.FixProposalSnapshot{
				ProposalID: m.FixProposal.ProposalID,
				RuleID:     m.FixProposal.RuleID,
				FixType:    m.FixProposal.FixType,
				BlockLabel: m.FixProposal.BlockLabel,
				Line:       m.FixProposal.Line,
				Summary:    m.FixProposal.Summary,
				Status:     m.FixProposal.Status,
				Message:    m.FixProposal.Message,
			}
			out[i].FixProposal.Items = convertItemsToModel(m.FixProposal.Items)
		}
		if len(m.FixProposals) > 0 {
			proposals := make([]models.FixProposalSnapshot, len(m.FixProposals))
			for j, fp := range m.FixProposals {
				proposals[j] = models.FixProposalSnapshot{
					ProposalID: fp.ProposalID,
					RuleID:     fp.RuleID,
					FixType:    fp.FixType,
					BlockLabel: fp.BlockLabel,
					Line:       fp.Line,
					Summary:    fp.Summary,
					Status:     fp.Status,
					Message:    fp.Message,
				}
				proposals[j].Items = convertItemsToModel(fp.Items)
			}
			out[i].FixProposals = proposals
		}
	}
	return out
}

// Per-conversation persistence limits enforced by the helpers below, used by
// the ChatService persistence path.
const (
	maxConvMessages     = 50
	maxConvBytes        = 1 * 1024 * 1024 // 1 MiB
	minRetainedConvMsgs = 4
)

// convFilePath resolves the on-disk path for a (scope, flowID) conversation,
// rejecting any component that could traverse outside the conversations
// directory. Both values flow straight into the path and scope can carry a
// flow-derived block ID, so they must be validated before use.
func convFilePath(configDir, scope, flowID string) (string, error) {
	if !safeConvComponent(scope) {
		return "", fmt.Errorf("invalid conversation scope %q", scope)
	}
	if !safeConvComponent(flowID) {
		return "", fmt.Errorf("invalid flow id %q", flowID)
	}
	return filepath.Join(configDir, "conversations", scope, flowID+".json"), nil
}

// safeConvComponent reports whether s is safe to use as a single path segment
// (no separators, no parent-dir escape, non-empty).
func safeConvComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, `/\`) && !strings.Contains(s, "..")
}

// evictConvMessages trims an oversized history to stay within the byte budget,
// never dropping below minRetainedConvMsgs recent turns. Each message is
// serialized once (O(n)) and suffix sums give any tail's size in O(1).
func evictConvMessages(messages []models.ChatMessage) []models.ChatMessage {
	if len(messages) <= maxConvMessages {
		return messages
	}
	n := len(messages)
	sizes := make([]int, n)
	for i := range messages {
		b, err := json.Marshal(messages[i])
		if err != nil {
			return messages // can't size it; leave the history untouched
		}
		sizes[i] = len(b)
	}
	suffix := make([]int, n+1)
	for i := n - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + sizes[i]
	}
	// marshaledLen(cut) == len(json.Marshal(messages[cut:])): a JSON array is
	// "[" + elements joined by "," + "]", i.e. 2 brackets + (k-1) commas.
	marshaledLen := func(cut int) int {
		k := n - cut
		if k <= 0 {
			return 2
		}
		return 2 + suffix[cut] + (k - 1)
	}
	if marshaledLen(0) <= maxConvBytes {
		return messages
	}
	cut := 0
	for cut+2 <= n-minRetainedConvMsgs {
		cut += 2
		if marshaledLen(cut) <= maxConvBytes && (n-cut) <= maxConvMessages {
			break
		}
	}
	return messages[cut:]
}

// atomicWriteConv writes data to a temp file in dir (mode 0600) and renames it
// over dest. A partial/interrupted write leaves the previous conversation intact
// rather than truncating the destination into an unparseable state.
func atomicWriteConv(dir, dest string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".conv-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp conversation file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp conversation file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp conversation file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp conversation file: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		cleanup()
		return fmt.Errorf("rename conversation file: %w", err)
	}
	return nil
}
