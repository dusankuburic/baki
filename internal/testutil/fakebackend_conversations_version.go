package testutil

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// conversationVersions is the fake's counterpart of migration 0044's sequence and trigger: one counter shared by
// every conversation, a new number for each created or changed conversation, never reused. A conversation without
// an entry (created before versions, or seeded straight into Conversations) is at version 0, as a legacy row is.
// It also keeps migration 0052's columns (convMeta): the owner, the title and the two times. A conversation seeded
// straight into Conversations has none: unowned, as a row stored before owners is. Every method runs under
// FakeBackend.mu.
type conversationVersions struct {
	seq  int64
	by   map[string]int64
	meta map[string]*convMeta
}

// convMeta is a conversation's owner_id, title, created_at and updated_at (migration 0052). Times are truncated to
// microseconds, as Postgres stores them, so a list cursor built from one compares as it does there.
type convMeta struct {
	owner, title         string
	createdAt, updatedAt time.Time
}

func (c *conversationVersions) next(key string) int64 {
	if c.by == nil {
		c.by = make(map[string]int64)
	}
	c.seq++
	c.by[key] = c.seq
	return c.seq
}

func (c *conversationVersions) drop(key string) {
	delete(c.by, key)
	delete(c.meta, key)
}

// metaFor is key's metadata, created (unowned) when it has none.
func (c *conversationVersions) metaFor(key string) *convMeta {
	if c.meta == nil {
		c.meta = make(map[string]*convMeta)
	}
	m := c.meta[key]
	if m == nil {
		m = &convMeta{}
		c.meta[key] = m
	}
	return m
}

func fakeNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// conversationVersionLocked is the version of the conversation at key: 0 when there is none.
func (m *FakeBackend) conversationVersionLocked(key string) (version int64, exists bool) {
	if _, ok := m.Conversations[key]; !ok {
		return 0, false
	}
	return m.convVersions.by[key], true
}

// conversationOwnerLocked is the owner of the conversation at key ("" for an unowned one) and whether it exists.
func (m *FakeBackend) conversationOwnerLocked(key string) (owner string, exists bool) {
	if _, ok := m.Conversations[key]; !ok {
		return "", false
	}
	if meta := m.convVersions.meta[key]; meta != nil {
		return meta.owner, true
	}
	return "", true
}

// reachesLocked is ownerReach: the system reaches every conversation, a user their own and the unowned ones.
func (m *FakeBackend) reachesLocked(key, owner string) bool {
	rowOwner, _ := m.conversationOwnerLocked(key)
	return owner == "" || rowOwner == "" || rowOwner == owner
}

// storeConversationLocked stores a copy of messages, giving the conversation a new version unless it already holds
// exactly this list. A created conversation is owner's ("" unowned) and its times are now; a changed one's updated_at
// is now.
func (m *FakeBackend) storeConversationLocked(key, owner string, cp []interfaces.ChatMessage) (int64, error) {
	if m.Conversations == nil {
		m.Conversations = make(map[string][]interfaces.ChatMessage)
	}
	old, exists := m.Conversations[key]
	if exists {
		same, err := sameMessages(old, cp)
		if err != nil {
			return 0, err
		}
		if same {
			return m.convVersions.by[key], nil
		}
	}
	now := fakeNow()
	if !exists {
		// A conversation deleted with its flow leaves no metadata behind to give a new one.
		delete(m.convVersions.meta, key)
		meta := m.convVersions.metaFor(key)
		meta.owner, meta.createdAt = owner, now
	}
	m.convVersions.metaFor(key).updatedAt = now
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

// LoadConversationVersioned returns the stored list and its version as owner sees it; none stored, or another
// user's, is an empty list at version 0.
func (m *FakeBackend) LoadConversationVersioned(_ context.Context, flowID, scope, owner string) ([]interfaces.ChatMessage, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := convKey(flowID, scope)
	if !m.reachesLocked(key, owner) {
		return []interfaces.ChatMessage{}, 0, nil
	}
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

// claimableLocked is the store's claim rule (interfaces.ConversationStore): an unowned conversation under a thread id
// that none of its stored messages names as its context block, for a write that names an owner.
func (m *FakeBackend) claimableLocked(key, scope, owner string) bool {
	rowOwner, exists := m.conversationOwnerLocked(key)
	return exists && rowOwner == "" && owner != "" && interfaces.IsThreadID(scope) &&
		!interfaces.NamesContextBlock(scope, m.Conversations[key])
}

// SaveConversationIf is the compare-and-swap: it stores messages only while the conversation is at expected and is
// owner's or unowned (claiming a claimable one); another user's conversation under the key is ErrConversationKeyTaken.
func (m *FakeBackend) SaveConversationIf(_ context.Context, flowID, scope, owner string, expected int64, messages []interfaces.ChatMessage) (int64, error) {
	cp, err := jsonClone(messages)
	if err != nil {
		return 0, fmt.Errorf("marshal messages: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := convKey(flowID, scope)
	if !m.reachesLocked(key, owner) {
		return 0, interfaces.ErrConversationKeyTaken
	}
	if version, _ := m.conversationVersionLocked(key); version != expected {
		return version, interfaces.ErrConversationConflict
	}
	if m.claimableLocked(key, scope, owner) {
		m.convVersions.metaFor(key).owner = owner
	}
	return m.storeConversationLocked(key, owner, cp)
}

// DeleteConversationIf removes the conversation only while it is at expected and is owner's, claimable, or (with
// mayDeleteUnowned) shared; none stored, or another user's, matches expected 0 only.
func (m *FakeBackend) DeleteConversationIf(_ context.Context, flowID, scope, owner string, expected int64, mayDeleteUnowned bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := convKey(flowID, scope)
	rowOwner, exists := m.conversationOwnerLocked(key)
	switch {
	case !exists, owner != "" && rowOwner != "" && rowOwner != owner:
		if expected != 0 {
			return interfaces.ErrConversationConflict
		}
		return nil
	case owner != "" && rowOwner == "" && !mayDeleteUnowned && !m.claimableLocked(key, scope, owner):
		return interfaces.ErrConversationUnowned
	}
	if version, _ := m.conversationVersionLocked(key); version != expected {
		return interfaces.ErrConversationConflict
	}
	delete(m.Conversations, key)
	m.convVersions.drop(key)
	return nil
}

// IsSharedConversation reports whether an unowned conversation that no write claims is stored under the key.
func (m *FakeBackend) IsSharedConversation(_ context.Context, flowID, scope string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := convKey(flowID, scope)
	rowOwner, exists := m.conversationOwnerLocked(key)
	return exists && rowOwner == "" && !m.claimableLocked(key, scope, "-"), nil
}

// ListConversations is owner's conversations on the flow, (updated_at, scope) descending, after the cursor.
func (m *FakeBackend) ListConversations(_ context.Context, flowID, owner string, after *interfaces.PageCursor, limit int) ([]interfaces.ConversationSummary, error) {
	if owner == "" || limit <= 0 {
		return nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []interfaces.ConversationSummary{}
	for key, msgs := range m.Conversations {
		scope, fid, _ := strings.Cut(key, "\x00")
		meta := m.convVersions.meta[key]
		if fid != flowID || meta == nil || meta.owner != owner {
			continue
		}
		if after != nil && (meta.updatedAt.After(after.At) || meta.updatedAt.Equal(after.At) && scope >= after.ID) {
			continue
		}
		out = append(out, interfaces.ConversationSummary{
			Scope: scope, Title: meta.title, CreatedAt: meta.createdAt, UpdatedAt: meta.updatedAt,
			MessageCount: len(msgs), Version: m.convVersions.by[key],
		})
	}
	slices.SortFunc(out, func(a, b interfaces.ConversationSummary) int {
		return cmp.Or(b.UpdatedAt.Compare(a.UpdatedAt), strings.Compare(b.Scope, a.Scope))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SetConversationTitle names owner's conversation (claiming a claimable one), creating it empty when a thread id
// holds none (any other missing key: ErrNotFound); the version and updated_at stay. It returns the version.
func (m *FakeBackend) SetConversationTitle(_ context.Context, flowID, scope, owner, title string) (int64, error) {
	if owner == "" {
		return 0, interfaces.ErrConversationUnowned
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := convKey(flowID, scope)
	rowOwner, exists := m.conversationOwnerLocked(key)
	switch {
	case !exists && !interfaces.IsThreadID(scope):
		return 0, interfaces.ErrNotFound
	case !exists:
		if _, err := m.storeConversationLocked(key, owner, []interfaces.ChatMessage{}); err != nil {
			return 0, err
		}
	case m.claimableLocked(key, scope, owner):
		m.convVersions.metaFor(key).owner = owner
	case rowOwner == "":
		return 0, interfaces.ErrConversationUnowned
	case rowOwner != owner:
		return 0, interfaces.ErrConversationKeyTaken
	}
	m.convVersions.metaFor(key).title = title
	return m.convVersions.by[key], nil
}
