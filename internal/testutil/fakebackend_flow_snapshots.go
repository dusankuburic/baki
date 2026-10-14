package testutil

import (
	"context"
	"maps"
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
	if lim.MaxBytes > 0 && s.StoredBytes() > lim.MaxBytes {
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
	fs.rings[s.FlowID] = ring[cut:]
	return nil
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
	delete(m.flowSnapshots.rings, flowID)
	m.flowSnapshots.mu.Unlock()
}

// purgeFlowSnapshots deletes the expired snapshots and returns how many; the caller holds m.mu.
func (m *FakeBackend) purgeFlowSnapshots(now time.Time) int {
	fs := &m.flowSnapshots
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := 0
	for id, ring := range fs.rings {
		before := len(ring)
		ring = deleteSnapshotsWhere(ring, func(s *interfaces.FlowSnapshot) bool { return flowSnapshotExpired(s, now) })
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
