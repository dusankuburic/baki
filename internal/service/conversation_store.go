package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pad-core/models"
)

// Per-conversation persistence limits. These (and the helpers below) consolidate
// the write/eviction logic that previously lived in a separate, unused
// internal/ai/conversation.go — the ChatService methods are the live persistence
// path, so the robustness belongs here.
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
	cleanup := func() { os.Remove(tmpName) }

	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp conversation file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
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
