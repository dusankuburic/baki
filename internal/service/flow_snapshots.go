package service

import (
	"context"
	"fmt"
	lru "github.com/hashicorp/golang-lru/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pad-core/logger"
	"pad-core/models"
	"pad-core/parser"

	"github.com/google/uuid"
)

// maxSnapshotsPerFlow bounds the in-memory undo ring: the session's last N pre-mutation states per flow
// (cloud mode has durable versions for real history).
const maxSnapshotsPerFlow = 10

// maxSnapshotFlows bounds how many flows keep undo rings (B1.1 LRU).
const maxSnapshotFlows = 64

// maxSnapshotBytes bounds the source bytes the WHOLE store retains: the count bounds alone allowed 640 full
// source copies (6+ GiB at the 10 MiB request cap) for what is only an undo affordance. 256 MiB keeps every
// ring 10 deep for flows up to ~400 KiB; past it the LRU ring's oldest snapshots go first (snapshotStore.push).
// That is the DESKTOP budget; in cloud mode maxSnapshotBytesCloud keeps the ring at 8% of GOMEMLIMIT
// (flow_versions are explicit "save version" rows only, so this ring IS the undo of a cloud fix).
const (
	maxSnapshotBytes      int64 = 256 << 20
	maxSnapshotBytesCloud int64 = 64 << 20
)

// SourceSnapshot is one pre-mutation state of a flow's source text, session-scoped (in-memory) — the undo
// affordance for "that fix made it worse", not a version-control system.
type SourceSnapshot struct {
	ID        string    `json:"id"`
	FlowID    string    `json:"flowId"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"createdAt"`
	// Bytes is the source text captured; omitted from list JSON (size only).
	Bytes int64 `json:"bytes"`
	// TargetPath is the desktop file the snapshot restores to ("" = cloud: via the stored-source persist path).
	TargetPath string `json:"targetPath,omitempty"`

	// Files is the per-file source map for FOLDER snapshots (nil for single-file, which use `source`); restore
	// re-parses the map with ParseFiles so the folder structure survives the undo.
	Files map[string]string `json:"-"`

	source string `json:"-"`
	// retained is what the store charged against maxSnapshotBytes (fixed at push) — recorded rather than
	// recomputed so every release subtracts exactly what was added.
	retained int64
}

// retainedBytes is the heap a snapshot pins (Bytes, the display size, counts only member texts).
func (snap *SourceSnapshot) retainedBytes() int64 {
	n := int64(len(snap.source))
	for name, text := range snap.Files {
		n += int64(len(name) + len(text))
	}
	return n
}

// snapshotStore is the per-flow ring, keyed by desktop FilePath (stable across re-parses) or cloud flow ID.
// The map of rings is LRU-bounded (maxSnapshotFlows): touching a ring refreshes its recency, so an active
// flow's undo history never evicts while in use. total is the retained size of every snapshot in every ring,
// kept exact across pushes, trims, eviction and whole-ring removal.
type snapshotStore struct {
	mu    sync.Mutex
	rings *lru.Cache[string, *snapshotRing]
	// total and maxBytes are guarded by mu. maxBytes is maxSnapshotBytes; a
	// field so tests can exercise eviction without allocating 256 MiB.
	total    int64
	maxBytes int64
}

// snapshotRing is one flow's undo history, oldest first. The LRU holds a pointer so budget eviction can drop
// an entry in place: re-Add()ing a changed slice would refresh the ring's recency and move the next eviction
// onto a different flow's history.
type snapshotRing struct {
	snaps []*SourceSnapshot
	bytes int64 // sum of snaps' retained sizes
}

func newSnapshotStore() *snapshotStore {
	return newSnapshotStoreWithBudget(maxSnapshotBytes)
}

// newSnapshotStoreWithBudget builds the store with the given byte budget.
func newSnapshotStoreWithBudget(maxBytes int64) *snapshotStore {
	st := &snapshotStore{maxBytes: maxBytes}
	// Fires for capacity eviction (Add) and drop (Remove); every store path holds st.mu around those calls, so
	// the callback already runs under st.mu and must not lock it again.
	onEvict := func(_ string, r *snapshotRing) {
		st.total -= r.bytes
		// Empty the evicted ring too: golang-lru keeps the last evicted value in its eviction buffer until
		// overwritten, which would otherwise pin that ring's full sources.
		clear(r.snaps)
		r.snaps, r.bytes = nil, 0
	}
	// lru.NewWithEvict only errors on size <= 0.
	rings, err := lru.NewWithEvict[string, *snapshotRing](maxSnapshotFlows, onEvict)
	if err != nil {
		panic(err)
	}
	st.rings = rings
	return st
}

// push records snap as key's newest entry, then enforces the per-flow count
// and the store-wide byte budget.
func (st *snapshotStore) push(key string, snap *SourceSnapshot) {
	st.mu.Lock()
	defer st.mu.Unlock()
	snap.retained = snap.retainedBytes()
	if snap.retained > st.maxBytes {
		// Larger than the whole budget: storing it would evict every other flow's history and then itself;
		// the ring's older entries stay valid restore points.
		logger.Warn("source snapshot exceeds the undo budget; not kept", "flow", snap.FlowID, "bytes", snap.retained, "budget", st.maxBytes)
		return
	}
	r, ok := st.rings.Get(key)
	if !ok {
		r = &snapshotRing{}
		st.rings.Add(key, r) // may evict the LRU ring (onEvict settles total)
	}
	r.snaps = append(r.snaps, snap)
	r.bytes += snap.retained
	st.total += snap.retained
	if n := len(r.snaps) - maxSnapshotsPerFlow; n > 0 {
		st.dropOldest(r, n)
	}
	// Budget: evict the LRU ring's oldest entry until under. key's ring was just refreshed, so it is the
	// oldest only when it is the last ring left — and its newest entry (snap) is never the one dropped.
	for st.total > st.maxBytes {
		oldestKey, oldest, ok := st.rings.GetOldest()
		if !ok {
			break
		}
		if len(oldest.snaps) <= 1 {
			st.rings.Remove(oldestKey) // onEvict settles total
			continue
		}
		st.dropOldest(oldest, 1)
	}
}

// dropOldest removes r's n oldest snapshots IN PLACE (survivors shift forward, tail cleared): a plain reslice
// left the dropped pointers in the backing array ahead of the slice, and append's growth kept that array alive,
// so a 10-entry ring pinned up to ~19 full sources. Caller holds st.mu.
func (st *snapshotStore) dropOldest(r *snapshotRing, n int) {
	for _, old := range r.snaps[:n] {
		r.bytes -= old.retained
		st.total -= old.retained
	}
	kept := copy(r.snaps, r.snaps[n:])
	clear(r.snaps[kept:])
	r.snaps = r.snaps[:kept]
}

func (st *snapshotStore) list(key string) []*SourceSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.rings.Get(key)
	if !ok {
		return nil
	}
	out := make([]*SourceSnapshot, len(r.snaps))
	copy(out, r.snaps)
	return out
}

func (st *snapshotStore) take(key, id string) (*SourceSnapshot, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.rings.Get(key)
	if !ok {
		return nil, false
	}
	for _, s := range r.snaps {
		if s.ID == id {
			return s, true
		}
	}
	return nil, false
}

// drop discards a flow's ring (flow deleted / cloud eviction).
func (st *snapshotStore) drop(key string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rings.Remove(key) // onEvict settles total
}

// flowSnapshotKey returns the stable per-flow identity for the undo ring.
func flowSnapshotKey(doc *models.FlowDocument) string {
	if doc.FilePath != "" {
		return doc.FilePath
	}
	return doc.ID
}

// The snapshot* builders below CAPTURE a pre-mutation state without recording it: the caller hands it to
// pushSnapshot only once the write persisted. They used to push on capture, so a mutation that was then
// rejected (OCC conflict, parse-error gate, write error) still consumed an undo slot and pinned a full source
// copy. Each returns nil when there is nothing to capture; pushSnapshot ignores nil.

// pushSnapshot records snap in doc's undo ring — only after the mutation it precedes has persisted.
func (s *FlowService) pushSnapshot(doc *models.FlowDocument, snap *SourceSnapshot) {
	if s.snapshots == nil || snap == nil {
		return
	}
	s.snapshots.push(flowSnapshotKey(doc), snap)
}

// snapshotDesktopFile captures the CURRENT bytes of targetPath before a
// mutation writes them. Missing file → nil (nothing to undo into).
func (s *FlowService) snapshotDesktopFile(doc *models.FlowDocument, targetPath, label string) *SourceSnapshot {
	if s.snapshots == nil {
		return nil
	}
	data, err := os.ReadFile(targetPath) // #nosec G304 -- path derived from doc.FilePath like the mutation itself
	if err != nil {
		return nil // pre-first-save or externally deleted: nothing to capture
	}
	return &SourceSnapshot{
		ID:         uuid.NewString(),
		FlowID:     doc.ID,
		Label:      label,
		CreatedAt:  time.Now().UTC(),
		Bytes:      int64(len(data)),
		TargetPath: targetPath,
		source:     string(data),
	}
}

// snapshotCloudSource captures the flow's current source (stored, or serialized for ingested flows).
func (s *FlowService) snapshotCloudSource(doc *models.FlowDocument, label string) *SourceSnapshot {
	source := doc.Source
	if source == "" {
		source = parser.SerializeDocument(doc)
	}
	return s.snapshotCloudSourceText(doc, source, label)
}

// snapshotCloudSourceText is snapshotCloudSource for a caller already holding the canonical source text: the
// fix paths derive it anyway, and re-serializing walked the whole block tree a second time per fix.
func (s *FlowService) snapshotCloudSourceText(doc *models.FlowDocument, source, label string) *SourceSnapshot {
	if s.snapshots == nil || source == "" {
		return nil
	}
	return &SourceSnapshot{
		ID:        uuid.NewString(),
		FlowID:    doc.ID,
		Label:     label,
		CreatedAt: time.Now().UTC(),
		Bytes:     int64(len(source)),
		source:    source,
	}
}

// snapshotCloudFiles captures a FOLDER flow's per-file sources before a multi-file mutation.
func (s *FlowService) snapshotCloudFiles(doc *models.FlowDocument, files map[string]string, label string) *SourceSnapshot {
	if s.snapshots == nil || len(files) == 0 {
		return nil
	}
	var total int64
	cp := make(map[string]string, len(files))
	for name, text := range files {
		total += int64(len(text))
		cp[name] = text
	}
	return &SourceSnapshot{
		ID:        uuid.NewString(),
		FlowID:    doc.ID,
		Label:     label,
		CreatedAt: time.Now().UTC(),
		Bytes:     total,
		Files:     cp,
	}
}

// snapshotDesktopFiles captures the pre-mutation bytes of the member files a desktop FOLDER batch is about to
// rewrite, as ONE undo entry; restore writes each file back and re-parses the folder.
func (s *FlowService) snapshotDesktopFiles(doc *models.FlowDocument, files map[string]string, label string) *SourceSnapshot {
	if s.snapshots == nil || len(files) == 0 || doc.FilePath == "" {
		return nil
	}
	var total int64
	cp := make(map[string]string, len(files))
	for name, text := range files {
		total += int64(len(text))
		cp[name] = text
	}
	return &SourceSnapshot{
		ID:         uuid.NewString(),
		FlowID:     doc.ID,
		Label:      label,
		CreatedAt:  time.Now().UTC(),
		Bytes:      total,
		TargetPath: doc.FilePath,
		Files:      cp,
	}
}

// ListSourceSnapshots returns the flow's undo ring, newest last.
func (s *FlowService) ListSourceSnapshots(doc *models.FlowDocument) []*SourceSnapshot {
	if s.snapshots == nil || doc == nil {
		return nil
	}
	return s.snapshots.list(flowSnapshotKey(doc))
}

// RestoreSourceSnapshot writes the snapshot's bytes back: desktop re-writes the recorded file; cloud persists
// through the stored-source path. Returns the re-loaded document.
func (s *FlowService) RestoreSourceSnapshot(ctx context.Context, doc *models.FlowDocument, snapshotID string) (*models.FlowDocument, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	if s.snapshots == nil {
		return nil, fmt.Errorf("no snapshots available")
	}
	snap, ok := s.snapshots.take(flowSnapshotKey(doc), snapshotID)
	if !ok {
		return nil, fmt.Errorf("snapshot %q not found for this flow", snapshotID)
	}

	if snap.TargetPath != "" && snap.Files != nil {
		// Desktop FOLDER batch: write every recorded member back, then re-parse once. The current bytes are
		// snapshotted first so the restore is itself undoable — recorded once a member was actually rewritten
		// (a later write failure leaves the earlier ones changed, and those must stay undoable).
		current := make(map[string]string, len(snap.Files))
		for name := range snap.Files {
			// On its own line: gosec ties the annotation to the statement it ends, not to an if's init clause.
			data, rerr := os.ReadFile(filepath.Join(snap.TargetPath, name)) // #nosec G304 -- recorded member of the flow folder
			if rerr == nil {
				current[name] = string(data)
			}
		}
		before := s.snapshotDesktopFiles(doc, current, "before restore")
		wrote := false
		for name, text := range snap.Files {
			target := filepath.Join(snap.TargetPath, name)
			if err := atomicWriteConv(snap.TargetPath, target, []byte(text)); err != nil {
				if wrote {
					s.pushSnapshot(doc, before)
				}
				return nil, fmt.Errorf("restore snapshot (%s): %w", name, err)
			}
			wrote = true
		}
		s.pushSnapshot(doc, before)
		s.publishDesktopChange(ctx, doc)
		logger.Info("folder snapshot restored", "flow", doc.ID, "snapshot", snapshotID, "label", snap.Label)
		return s.LoadFlowFolder(snap.TargetPath)
	}

	if snap.TargetPath != "" {
		// Desktop: restore the recorded file; the CURRENT bytes are snapshotted first so the restore is
		// itself undoable, recorded once the write landed.
		before := s.snapshotDesktopFile(doc, snap.TargetPath, "before restore")
		if err := atomicWriteConv(filepath.Dir(snap.TargetPath), snap.TargetPath, []byte(snap.source)); err != nil {
			return nil, fmt.Errorf("restore snapshot: %w", err)
		}
		s.pushSnapshot(doc, before)
		// The restored bytes replaced the file: derived caches are stale.
		s.publishDesktopChange(ctx, doc)
		if doc.FilePath != "" {
			if info, serr := os.Stat(doc.FilePath); serr == nil && info.IsDir() {
				return s.LoadFlowFolder(doc.FilePath)
			}
			return s.LoadFlowFromPath(doc.FilePath)
		}
		// Snapshot recorded a file but the doc has no FilePath anymore (switched docs?): report via parse.
		restored, perr := parser.ParseText(snap.source, filepath.Base(snap.TargetPath), snap.Bytes)
		if perr != nil {
			return nil, fmt.Errorf("re-parse restored source: %w", perr)
		}
		return restored, nil
	}

	// Folder snapshot: re-parse the per-file map and persist through the same OCC path (a folder restored as a
	// single combined text would collapse the structure — the hazard SaveSource guards against). The current
	// state is snapshotted first, so the restore is itself undoable.
	if snap.Files != nil {
		before := s.snapshotCloudFiles(doc, parser.SerializeFiles(doc), "before restore")
		combined := parseFilesPreservingIdentity(snap.Files, doc)
		if combined == nil {
			return nil, fmt.Errorf("restore: re-parse snapshot files failed")
		}
		updated, err := s.persistCloudDoc(ctx, doc, combined, "")
		if err != nil {
			return nil, err
		}
		s.pushSnapshot(doc, before)
		logger.Info("folder snapshot restored", "flow", doc.ID, "snapshot", snapshotID, "label", snap.Label)
		return updated, nil
	}

	// Cloud: persist the old source through the standard path (OCC, re-parse, SaveFlow); the CURRENT state is
	// snapshotted first so the restore itself is undoable.
	before := s.snapshotCloudSource(doc, "before restore")
	updated, err := s.persistCloudSource(ctx, doc, snap.source)
	if err != nil {
		return nil, err
	}
	s.pushSnapshot(doc, before)
	logger.Info("source snapshot restored", "flow", doc.ID, "snapshot", snapshotID, "label", snap.Label)
	return updated, nil
}

// SourceMeta is the cheap change-detection signal for the desktop watcher: size + mtime (folders report the
// combined max mtime + total size so ANY member edit registers).
type SourceMeta struct {
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
	// File count for folder flows (1 for single files); 0 when no local file.
	Files int `json:"files"`
}

// GetSourceMeta stats the flow's backing file(s) without reading content; cloud flows return Files=0.
func (s *FlowService) GetSourceMeta(doc *models.FlowDocument) (*SourceMeta, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	if doc.FilePath == "" {
		return &SourceMeta{Files: 0}, nil
	}
	info, err := os.Stat(doc.FilePath)
	if err != nil {
		// The watcher treats stat failure as "gone" (Files=0), not an error: a file deleted between polls is
		// a normal editor save dance.
		return &SourceMeta{Files: 0}, nil
	}
	if !info.IsDir() {
		return &SourceMeta{Size: info.Size(), ModTime: info.ModTime().UTC().Format(time.RFC3339Nano), Files: 1}, nil
	}
	// Folder: aggregate member files (the same *.txt/*.pad set loadFolder consumes) so any edit moves the numbers.
	var total int64
	var latest time.Time
	entries, err := os.ReadDir(doc.FilePath)
	if err != nil {
		return &SourceMeta{Files: 0}, nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".txt" && ext != ".pad" {
			continue
		}
		fi, serr := e.Info()
		if serr != nil {
			continue
		}
		total += fi.Size()
		if fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return &SourceMeta{Size: total, ModTime: latest.UTC().Format(time.RFC3339Nano), Files: len(entries)}, nil
}
