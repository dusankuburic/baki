package testutil

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// fakeFlowSnapshots backs the cloud undo ring (flow_snapshots, migration 0045): each flow's snapshots in push
// order. Its own lock, taken after FakeBackend.mu wherever both are held (a push checks the flow under mu, a flow
// delete and the purge run under it), so a snapshot never outlives its flow.
type fakeFlowSnapshots struct {
	mu    sync.Mutex
	rings map[string][]*interfaces.FlowSnapshot
	// seq is each stored snapshot's push order across every flow (the table's BIGSERIAL), which the per-user bound
	// trims by; next is the last one handed out.
	seq  map[*interfaces.FlowSnapshot]int64
	next int64
}

// PushFlowSnapshot mirrors the Postgres push: the flow must exist, the text must be storable, and the ring is
// trimmed to lim afterwards (newest first by count, then by stored bytes).
func (m *FakeBackend) PushFlowSnapshot(_ context.Context, s *interfaces.FlowSnapshot, lim interfaces.FlowSnapshotLimits) error {
	if err := validFlowID(s.FlowID); err != nil {
		return err
	}
	if strings.Contains(s.Source, "\x00") {
		return interfaces.ErrFlowSnapshotUnstorable
	}
	for name, text := range s.Files {
		if strings.Contains(name, "\x00") || strings.Contains(text, "\x00") {
			return interfaces.ErrFlowSnapshotUnstorable
		}
	}
	if (lim.MaxBytes > 0 && s.StoredBytes() > lim.MaxBytes) || (s.UserID != "" && lim.UserMaxBytes > 0 && s.StoredBytes() > lim.UserMaxBytes) {
		return interfaces.ErrFlowSnapshotTooLarge
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.Flows[s.FlowID]; !ok {
		return interfaces.ErrNotFound
	}
	cp := *s
	cp.Files = maps.Clone(s.Files)
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	cp.CreatedAt = pgTime(cp.CreatedAt)

	fs := &m.flowSnapshots
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.rings == nil {
		fs.rings = map[string][]*interfaces.FlowSnapshot{}
	}
	if fs.seq == nil {
		fs.seq = map[*interfaces.FlowSnapshot]int64{}
	}
	fs.next++
	fs.seq[&cp] = fs.next
	ring := append(fs.rings[s.FlowID], &cp)
	// Walk newest first: keep while within the count and the byte bound, drop the rest.
	var kept, bytes int64
	cut := 0
	for i := len(ring) - 1; i >= 0; i-- {
		bytes += ring[i].StoredBytes()
		kept++
		if (lim.Keep > 0 && kept > int64(lim.Keep)) || (lim.MaxBytes > 0 && bytes > lim.MaxBytes) {
			cut = i + 1
			break
		}
	}
	for _, gone := range ring[:cut] {
		delete(fs.seq, gone)
	}
	fs.rings[s.FlowID] = ring[cut:]
	if s.UserID != "" && (lim.UserKeep > 0 || lim.UserMaxBytes > 0) {
		fs.trimUser(s.UserID, lim)
	}
	return nil
}

// trimUser applies the per-user bound: the user's snapshots on every flow, newest first in push order, are kept
// while within the count and the bytes, the rest deleted. The caller holds fs.mu. Postgres skips (neither counts nor
// deletes) a row a concurrent push holds and catches up at the next push; pushes here are serialized by fs.mu, so no
// row is ever held and the bound is exact, the answer Postgres gives once concurrent pushes have committed.
func (fs *fakeFlowSnapshots) trimUser(userID string, lim interfaces.FlowSnapshotLimits) {
	var mine []*interfaces.FlowSnapshot
	for _, ring := range fs.rings {
		for _, s := range ring {
			if s.UserID == userID {
				mine = append(mine, s)
			}
		}
	}
	slices.SortFunc(mine, func(a, b *interfaces.FlowSnapshot) int { return cmp.Compare(fs.seq[b], fs.seq[a]) })
	drop := map[*interfaces.FlowSnapshot]bool{}
	var kept, bytes int64
	for _, s := range mine {
		bytes += s.StoredBytes()
		kept++
		if (lim.UserKeep > 0 && kept > int64(lim.UserKeep)) || (lim.UserMaxBytes > 0 && bytes > lim.UserMaxBytes) {
			drop[s] = true
		}
	}
	if len(drop) == 0 {
		return
	}
	for id, ring := range fs.rings {
		if ring = deleteSnapshotsWhere(ring, func(s *interfaces.FlowSnapshot) bool { return drop[s] }); len(ring) == 0 {
			delete(fs.rings, id)
		} else {
			fs.rings[id] = ring
		}
	}
	for s := range drop {
		delete(fs.seq, s)
	}
}

// ListFlowSnapshots returns the flow's live snapshots oldest first, without their text.
func (m *FakeBackend) ListFlowSnapshots(_ context.Context, flowID string) ([]*interfaces.FlowSnapshot, error) {
	fs := &m.flowSnapshots
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := []*interfaces.FlowSnapshot{}
	for _, s := range fs.rings[flowID] {
		if flowSnapshotExpired(s, time.Now()) {
			continue
		}
		cp := *s
		cp.Source, cp.Files = "", nil
		out = append(out, &cp)
	}
	return out, nil
}

// LoadFlowSnapshot returns the live snapshot id of the flow with its text.
func (m *FakeBackend) LoadFlowSnapshot(_ context.Context, flowID, id string) (*interfaces.FlowSnapshot, error) {
	fs := &m.flowSnapshots
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, s := range fs.rings[flowID] {
		if s.ID == id && !flowSnapshotExpired(s, time.Now()) {
			cp := *s
			cp.Files = maps.Clone(s.Files)
			return &cp, nil
		}
	}
	return nil, interfaces.ErrNotFound
}

func flowSnapshotExpired(s *interfaces.FlowSnapshot, now time.Time) bool {
	return !s.CreatedAt.After(now.Add(-interfaces.FlowSnapshotTTL))
}

// dropFlowSnapshots is the ON DELETE CASCADE; the caller holds m.mu.
func (m *FakeBackend) dropFlowSnapshots(flowID string) {
	m.flowSnapshots.mu.Lock()
	for _, s := range m.flowSnapshots.rings[flowID] {
		delete(m.flowSnapshots.seq, s)
	}
	delete(m.flowSnapshots.rings, flowID)
	m.flowSnapshots.mu.Unlock()
}

// eraseFlowSnapshotUserLocked is the erasure's identity step: the user's snapshots on other people's flows stay
// (the text is the flow's) without naming the user. The caller holds m.mu.
func (m *FakeBackend) eraseFlowSnapshotUserLocked(userID string) {
	m.flowSnapshots.mu.Lock()
	defer m.flowSnapshots.mu.Unlock()
	for _, ring := range m.flowSnapshots.rings {
		for _, s := range ring {
			if s.UserID == userID {
				s.UserID = ""
			}
		}
	}
}

// purgeFlowSnapshots deletes the expired snapshots and returns how many; the caller holds m.mu.
func (m *FakeBackend) purgeFlowSnapshots(now time.Time) int {
	fs := &m.flowSnapshots
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := 0
	for id, ring := range fs.rings {
		before := len(ring)
		ring = deleteSnapshotsWhere(ring, func(s *interfaces.FlowSnapshot) bool {
			if flowSnapshotExpired(s, now) {
				delete(fs.seq, s)
				return true
			}
			return false
		})
		n += before - len(ring)
		if len(ring) == 0 {
			delete(fs.rings, id)
		} else {
			fs.rings[id] = ring
		}
	}
	return n
}

func deleteSnapshotsWhere(ring []*interfaces.FlowSnapshot, del func(*interfaces.FlowSnapshot) bool) []*interfaces.FlowSnapshot {
	out := ring[:0:0]
	for _, s := range ring {
		if !del(s) {
			out = append(out, s)
		}
	}
	return out
}

var _ interfaces.FlowSnapshotStore = (*FakeBackend)(nil)
