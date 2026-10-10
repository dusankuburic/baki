package testutil

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// ---- Rule sets and "new rules" notices (ADR 0016) ----

func cloneRuleSet(rs *models.RuleSet) *models.RuleSet {
	if rs == nil {
		return nil
	}
	cp := *rs
	cp.Rules = slices.Clone(rs.Rules)
	return &cp
}

// RuleSetsByDigest resolves digests through the rule sets the stored baselines and snapshots record, baselines first.
func (m *FakeBackend) RuleSetsByDigest(_ context.Context, digests []string) (map[string]*models.RuleSet, error) {
	m.RuleSetsByDigestCalls.Add(1)
	out := make(map[string]*models.RuleSet, len(digests))
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, d := range digests {
		if _, done := out[d]; done || d == "" {
			continue
		}
		for _, id := range slices.Sorted(maps.Keys(m.Baselines)) {
			if bl := m.Baselines[id]; bl.RuleSet != nil && bl.RuleSetDigest == d {
				out[d] = cloneRuleSet(bl.RuleSet)
				break
			}
		}
		if out[d] != nil {
			continue
		}
		for _, id := range slices.Sorted(maps.Keys(m.snapshotRuleSets)) {
			if rs := m.snapshotRuleSets[id]; rs.Digest == d {
				out[d] = cloneRuleSet(rs)
				break
			}
		}
	}
	return out, nil
}

// ruleNoticeVisibleLocked is Postgres's ruleNoticeVisible: an org notice for the org's members, a user notice for
// that user, an app notice for every caller.
func (m *FakeBackend) ruleNoticeVisibleLocked(n *interfaces.RuleNotice, userID string) bool {
	switch n.Scope {
	case interfaces.RuleNoticeScopeApp:
		return true
	case interfaces.RuleNoticeScopeUser:
		return n.ScopeID == userID
	case interfaces.RuleNoticeScopeOrg:
		org, ok := m.Orgs[n.ScopeID]
		return ok && slices.ContainsFunc(org.Members, func(mem interfaces.OrgMember) bool { return mem.UserID == userID })
	}
	return false
}

// UpsertRuleNotices inserts new ids and merges known ones (rule-id union, larger flow count, stamps kept), skipping a
// notice whose user or organisation does not exist, as the Postgres statement does. Returns the inserted ids, sorted.
func (m *FakeBackend) UpsertRuleNotices(_ context.Context, notices []*interfaces.RuleNotice) ([]string, error) {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.RuleNotices == nil {
		m.RuleNotices = make(map[string]*interfaces.RuleNotice)
	}
	for _, n := range notices {
		if n == nil {
			continue
		}
		if err := validFakeRuleNotice(n); err != nil {
			return nil, err
		}
	}
	var inserted []string
	for _, n := range notices {
		if n == nil {
			continue
		}
		switch {
		case n.Scope == interfaces.RuleNoticeScopeUser && m.Users[n.ScopeID] == nil,
			n.Scope == interfaces.RuleNoticeScopeOrg && m.Orgs[n.ScopeID] == nil:
			continue
		}
		ids := slices.Compact(slices.Sorted(slices.Values(n.RuleIDs)))
		if ids == nil {
			ids = []string{}
		}
		if cur, ok := m.RuleNotices[n.ID]; ok {
			cur.RuleIDs = slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(cur.RuleIDs), ids...))))
			cur.FlowCount = max(cur.FlowCount, n.FlowCount)
			cur.UpdatedAt = now
			continue
		}
		m.RuleNotices[n.ID] = &interfaces.RuleNotice{
			ID: n.ID, Kind: n.Kind, Scope: n.Scope, ScopeID: n.ScopeID, RuleSetDigest: n.RuleSetDigest,
			RuleIDs: ids, FlowCount: n.FlowCount, CreatedAt: now, UpdatedAt: now,
		}
		inserted = append(inserted, n.ID)
	}
	slices.Sort(inserted)
	return slices.Compact(inserted), nil
}

// validFakeRuleNotice is the Postgres backend's validRuleNotice.
func validFakeRuleNotice(n *interfaces.RuleNotice) error {
	switch {
	case n.Kind == "":
		return fmt.Errorf("rule notice %q: kind is required", n.ID)
	case n.Scope != interfaces.RuleNoticeScopeOrg && n.Scope != interfaces.RuleNoticeScopeUser && n.Scope != interfaces.RuleNoticeScopeApp:
		return fmt.Errorf("rule notice %q: unknown scope %q", n.ID, n.Scope)
	case (n.Scope == interfaces.RuleNoticeScopeApp) != (n.ScopeID == ""):
		return fmt.Errorf("rule notice %q: scope %q with scope id %q", n.ID, n.Scope, n.ScopeID)
	case n.RuleSetDigest == "":
		return fmt.Errorf("rule notice %q: rule-set digest is required", n.ID)
	case n.ID != interfaces.RuleNoticeID(n.Scope, n.ScopeID, n.RuleSetDigest):
		return fmt.Errorf("rule notice %q: id is not RuleNoticeID(%q, %q, %q)", n.ID, n.Scope, n.ScopeID, n.RuleSetDigest)
	}
	return nil
}

// ListRuleNotices returns the caller's visible notices that are not dismissed, newest first (id breaking ties); a
// limit <= 0 is 10.
func (m *FakeBackend) ListRuleNotices(_ context.Context, userID string, limit int) ([]*interfaces.RuleNotice, error) {
	if limit <= 0 {
		limit = 10
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*interfaces.RuleNotice, 0)
	for _, n := range m.RuleNotices {
		if n.DismissedAt != nil || !m.ruleNoticeVisibleLocked(n, userID) {
			continue
		}
		cp := *n
		cp.RuleIDs = slices.Clone(n.RuleIDs)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return strings.Compare(out[i].ID, out[j].ID) < 0
	})
	return out[:min(limit, len(out))], nil
}

func (m *FakeBackend) UnreadRuleNoticeCount(_ context.Context, userID string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, rn := range m.RuleNotices {
		if rn.ReadAt == nil && rn.DismissedAt == nil && m.ruleNoticeVisibleLocked(rn, userID) {
			n++
		}
	}
	return n, nil
}

func (m *FakeBackend) MarkAllRuleNoticesRead(_ context.Context, userID string) error {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.RuleNotices {
		if n.ReadAt == nil && n.DismissedAt == nil && m.ruleNoticeVisibleLocked(n, userID) {
			n.ReadAt = &now
		}
	}
	return nil
}

func (m *FakeBackend) DismissRuleNotice(_ context.Context, userID, id string) error {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.RuleNotices[id]; ok && n.DismissedAt == nil && m.ruleNoticeVisibleLocked(n, userID) {
		n.DismissedAt = &now
	}
	return nil
}

// RuleNoticesSnapshot returns a copy of every stored notice, by id, whoever may see it.
func (m *FakeBackend) RuleNoticesSnapshot() map[string]*interfaces.RuleNotice {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*interfaces.RuleNotice, len(m.RuleNotices))
	for id, n := range m.RuleNotices {
		cp := *n
		cp.RuleIDs = slices.Clone(n.RuleIDs)
		out[id] = &cp
	}
	return out
}
