package ai

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"pad-analyzer/internal/models"
)

var safeScopeRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

const (
	conversationVersion    = 1
	maxMessagesPerConv     = 50
	maxConversationBytes   = 1 * 1024 * 1024
	maxTotalStorageBytes   = 50 * 1024 * 1024
)

func ConversationDir(configDir string) string {
	return filepath.Join(configDir, "conversations")
}

func FlowKey(filePath string) string {
	h := sha256.Sum256([]byte(filePath))
	return fmt.Sprintf("%x", h[:8])
}

func validateScope(scope string) error {
	if scope != "flow" && !safeScopeRe.MatchString(scope) {
		return fmt.Errorf("invalid scope: %s", scope)
	}
	return nil
}

func SaveConversation(configDir, filePath string, scope string, messages []models.ChatMessage) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	baseDir := ConversationDir(configDir)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return fmt.Errorf("create conversations dir: %w", err)
	}

	flowDir := filepath.Join(baseDir, FlowKey(filePath))
	if err := os.MkdirAll(flowDir, 0755); err != nil {
		return fmt.Errorf("create flow conversation dir: %w", err)
	}

	evicted, err := evictIfNeeded(messages)
	if err == nil {
		messages = evicted
	}

	convFile := filepath.Join(flowDir, scope+".json")
	conv := models.ConversationFile{
		Version:   conversationVersion,
		FlowKey:   FlowKey(filePath),
		Scope:     scope,
		UpdatedAt: time.Now(),
		Messages:  messages,
	}

	data, err := json.MarshalIndent(conv, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal conversation: %w", err)
	}

	return os.WriteFile(convFile, data, 0600)
}

func LoadConversation(configDir, filePath string, scope string) (*models.ConversationFile, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	convFile := filepath.Join(ConversationDir(configDir), FlowKey(filePath), scope+".json")
	data, err := os.ReadFile(convFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &models.ConversationFile{
				Version:  conversationVersion,
				FlowKey:  FlowKey(filePath),
				Scope:    scope,
				Messages: []models.ChatMessage{},
			}, nil
		}
		return nil, fmt.Errorf("read conversation: %w", err)
	}

	var conv models.ConversationFile
	if err := json.Unmarshal(data, &conv); err != nil {
		return &models.ConversationFile{
			Version:  conversationVersion,
			FlowKey:  FlowKey(filePath),
			Scope:    scope,
			Messages: []models.ChatMessage{},
		}, nil
	}

	if conv.Version > conversationVersion {
		return &models.ConversationFile{
			Version:  conversationVersion,
			FlowKey:  FlowKey(filePath),
			Scope:    scope,
			Messages: []models.ChatMessage{},
		}, nil
	}

	return &conv, nil
}

func ClearAllConversations(configDir string) error {
	baseDir := ConversationDir(configDir)
	return os.RemoveAll(baseDir)
}

func evictIfNeeded(messages []models.ChatMessage) ([]models.ChatMessage, error) {
	if len(messages) <= maxMessagesPerConv {
		return messages, nil
	}

	// Marshal each message ONCE to get its serialized length, then derive the
	// size of any suffix in O(1) from a suffix-sum. Previously this re-marshalled
	// the shrinking slice on every loop iteration → O(n²) on the save path.
	n := len(messages)
	sizes := make([]int, n)
	for i := range messages {
		b, err := json.Marshal(messages[i])
		if err != nil {
			return messages, err
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
			return 2 // "[]"
		}
		return 2 + suffix[cut] + (k - 1)
	}

	if marshaledLen(0) <= maxConversationBytes {
		return messages, nil
	}

	cut := 0
	for cut < n-2 {
		cut += 2
		if marshaledLen(cut) <= maxConversationBytes && (n-cut) <= maxMessagesPerConv {
			break
		}
	}

	return messages[cut:], nil
}

func EvictOldConversations(configDir string) error {
	baseDir := ConversationDir(configDir)
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	type fileStat struct {
		path    string
		modTime time.Time
		size    int64
	}

	var files []fileStat
	var totalSize int64

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		flowDir := filepath.Join(baseDir, entry.Name())
		convEntries, err := os.ReadDir(flowDir)
		if err != nil {
			continue
		}
		for _, ce := range convEntries {
			if ce.IsDir() {
				continue
			}
			info, err := ce.Info()
			if err != nil {
				continue
			}
			totalSize += info.Size()
			files = append(files, fileStat{
				path:    filepath.Join(flowDir, ce.Name()),
				modTime: info.ModTime(),
				size:    info.Size(),
			})
		}
	}

	if totalSize <= maxTotalStorageBytes {
		return nil
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	for _, f := range files {
		if totalSize <= maxTotalStorageBytes {
			break
		}
		os.Remove(f.path)
		totalSize -= f.size
	}

	return nil
}
