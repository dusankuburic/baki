package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"pad-analyzer/internal/storage/interfaces"
)

// conversationVersions is the fake's counterpart of migration 0044's sequence and trigger: one counter shared by
// every conversation, a new number for each created or changed conversation, never reused. A conversation without
// an entry (created before versions, or seeded straight into Conversations) is at version 0, as a legacy row is.
// Every method runs under FakeBackend.mu.
type conversationVersions struct {
	seq int64
	by  map[string]int64
}

func (c *conversationVersions) next(key string) int64 {
	if c.by == nil {
		c.by = make(map[string]int64)
	}
	c.seq++
	c.by[key] = c.seq
	return c.seq
}

func (c *conversationVersions) drop(key string) { delete(c.by, key) }

// conversationVersionLocked is the version of the conversation at key: 0 when there is none.
func (m *FakeBackend) conversationVersionLocked(key string) (version int64, exists bool) {
	if _, ok := m.Conversations[key]; !ok {
		return 0, false
	}
	return m.convVersions.by[key], true
}

// storeConversationLocked stores a copy of messages, giving the conversation a new version unless it already holds
// exactly this list.
func (m *FakeBackend) storeConversationLocked(key string, cp []interfaces.ChatMessage) (int64, error) {
	if m.Conversations == nil {
		m.Conversations = make(map[string][]interfaces.ChatMessage)
	}
	if old, ok := m.Conversations[key]; ok {
		same, err := sameMessages(old, cp)
		if err != nil {
			return 0, err
		}
		if same {
			return m.convVersions.by[key], nil
		}
	}
	m.Conversations[key] = cp
	return m.convVersions.next(key), nil
}

func sameMessages(a, b []interfaces.ChatMessage) (bool, error) {
	ja, err := json.Marshal(a)
	if err != nil {
		return false, fmt.Errorf("marshal messages: %w", err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false, fmt.Errorf("marshal messages: %w", err)
	}
	return bytes.Equal(ja, jb), nil
}

// LoadConversationVersioned returns the stored list and its version; none stored is an empty list at version 0.
func (m *FakeBackend) LoadConversationVersioned(_ context.Context, flowID, scope string) ([]interfaces.ChatMessage, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := convKey(flowID, scope)
	msgs, err := jsonClone(m.Conversations[key])
	if err != nil {
		return nil, 0, fmt.Errorf("unmarshal messages: %w", err)
	}
	if msgs == nil {
		msgs = []interfaces.ChatMessage{}
	}
	version, _ := m.conversationVersionLocked(key)
	return msgs, version, nil
}

// SaveConversationIf is the compare-and-swap: it stores messages only while the conversation is at expected.
func (m *FakeBackend) SaveConversationIf(_ context.Context, flowID, scope string, expected int64, messages []interfaces.ChatMessage) (int64, error) {
	cp, err := jsonClone(messages)
	if err != nil {
		return 0, fmt.Errorf("marshal messages: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := convKey(flowID, scope)
	if version, _ := m.conversationVersionLocked(key); version != expected {
		return version, interfaces.ErrConversationConflict
	}
	return m.storeConversationLocked(key, cp)
}

// DeleteConversationIf removes the conversation only while it is at expected; none stored matches expected 0 only.
func (m *FakeBackend) DeleteConversationIf(_ context.Context, flowID, scope string, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := convKey(flowID, scope)
	if version, _ := m.conversationVersionLocked(key); version != expected {
		return interfaces.ErrConversationConflict
	}
	delete(m.Conversations, key)
	m.convVersions.drop(key)
	return nil
}
