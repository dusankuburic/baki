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

// maxSnapshotsPerFlow bounds the in-memory undo ring: a session's last N
// pre-mutation states per flow. Enough for "undo the last few fixes" without
// unbounded retention (cloud mode has durable versions for real history).
const maxSnapshotsPerFlow = 10

// maxSnapshotFlows bounds how many flows keep undo rings (B1.1 LRU).
const maxSnapshotFlows = 64

// SourceSnapshot is one pre-mutation state of a flow's source text: the
// bytes exactly as they were BEFORE a fix/batch/save wrote them. Session-
// scoped (in-memory) — the undo affordance for "that fix made it worse",
// not a version-control system.
type SourceSnapshot struct {
	ID        string    `json:"id"`
	FlowID    string    `json:"flowId"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"createdAt"`
	// Bytes is the source text captured; omitted from list JSON (size only).
	Bytes int64 `json:"bytes"`
	// TargetPath is the desktop file the snapshot restores to ("" = cloud:
	// restore goes through the stored-source persist path).
	TargetPath string `json:"targetPath,omitempty"`

	// Files is the per-file source map for FOLDER snapshots (nil for
	// single-file snapshots, which use `source`). Restore re-parses the map
	// with ParseFiles so the folder structure survives the undo.
	Files map[string]string `json:"-"`

	source string `json:"-"`
}

// snapshotStore is the per-flow ring. Keyed by desktop FilePath (stable
// across re-parses) or cloud flow ID.
//
// B1.1: the map of rings is LRU-bounded (maxSnapshotFlows) — it used to grow
// a key per flow ever mutated, each holding up to maxSnapshotsPerFlow full
// source copies FOREVER (folder snapshots copy every member file), an
// unbounded heap leak on long-lived instances. Touching a ring (push, list,
// take) refreshes its LRU position so an active flow's undo history never
// evicts while in use.
type snapshotStore struct {
	mu    sync.Mutex
	rings *lru.Cache[string, []*SourceSnapshot]
}

func newSnapshotStore() *snapshotStore {
	// lru.New only errors on size <= 0; the constant keeps this unreachable.
	rings, err := lru.New[string, []*SourceSnapshot](maxSnapshotFlows)
	if err != nil {
		panic(err)
	}
	return &snapshotStore{rings: rings}
}

func (st *snapshotStore) push(key string, snap *SourceSnapshot) []*SourceSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	ring, _ := st.rings.Get(key)
	ring = append(ring, snap)
	if len(ring) > maxSnapshotsPerFlow {
		ring = ring[len(ring)-maxSnapshotsPerFlow:]
	}
	st.rings.Add(key, ring)
	return ring
}

func (st *snapshotStore) list(key string) []*SourceSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	v, ok := st.rings.Get(key)
	if !ok {
		return nil
	}
	out := make([]*SourceSnapshot, len(v))
	copy(out, v)
	return out
}

func (st *snapshotStore) take(key, id string) (*SourceSnapshot, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	v, ok := st.rings.Get(key)
	if !ok {
		return nil, false
	}
	for _, s := range v {
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
	st.rings.Remove(key)
}

// flowSnapshotKey returns the stable per-flow identity for the undo ring.
func flowSnapshotKey(doc *models.FlowDocument) string {
	if doc.FilePath != "" {
		return doc.FilePath
	}
	return doc.ID
}

// snapshotDesktopFile captures the CURRENT bytes of targetPath before a
// mutation writes them. Missing file → no snapshot (nothing to undo into).
func (s *FlowService) snapshotDesktopFile(doc *models.FlowDocument, targetPath, label string) {
	if s.snapshots == nil {
		return
	}
	data, err := os.ReadFile(targetPath) // #nosec G304 -- path derived from doc.FilePath like the mutation itself
	if err != nil {
		return // pre-first-save or externally deleted: nothing to capture
	}
	s.snapshots.push(flowSnapshotKey(doc), &SourceSnapshot{
		ID:         uuid.NewString(),
		FlowID:     doc.ID,
		Label:      label,
		CreatedAt:  time.Now().UTC(),
		Bytes:      int64(len(data)),
		TargetPath: targetPath,
		source:     string(data),
	})
}

// snapshotCloudSource captures the flow's current source (stored, or the
// serialized form for ingested flows) before a cloud mutation persists a new
// one.
func (s *FlowService) snapshotCloudSource(doc *models.FlowDocument, label string) {
	if s.snapshots == nil {
		return
	}
	source := doc.Source
	if source == "" {
		source = parser.SerializeDocument(doc)
	}
	if source == "" {
		return
	}
	s.snapshots.push(flowSnapshotKey(doc), &SourceSnapshot{
		ID:        uuid.NewString(),
		FlowID:    doc.ID,
		Label:     label,
		CreatedAt: time.Now().UTC(),
		Bytes:     int64(len(source)),
		source:    source,
	})
}

// snapshotCloudFiles captures a FOLDER flow's per-file sources before a
// multi-file mutation (fix/batch). Bytes carries the combined size for list
// display; restore re-parses the map so the folder shape survives.
func (s *FlowService) snapshotCloudFiles(doc *models.FlowDocument, files map[string]string, label string) {
	if s.snapshots == nil || len(files) == 0 {
		return
	}
	var total int64
	cp := make(map[string]string, len(files))
	for name, text := range files {
		total += int64(len(text))
		cp[name] = text
	}
	s.snapshots.push(flowSnapshotKey(doc), &SourceSnapshot{
		ID:        uuid.NewString(),
		FlowID:    doc.ID,
		Label:     label,
		CreatedAt: time.Now().UTC(),
		Bytes:     total,
		Files:     cp,
	})
}

// ListSourceSnapshots returns the flow's undo ring, newest last.
func (s *FlowService) ListSourceSnapshots(doc *models.FlowDocument) []*SourceSnapshot {
	if s.snapshots == nil || doc == nil {
		return nil
	}
	return s.snapshots.list(flowSnapshotKey(doc))
}

// RestoreSourceSnapshot writes the snapshot's bytes back: desktop re-writes
// the recorded file; cloud persists through the stored-source path. Returns
// the re-loaded document.
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

	if snap.TargetPath != "" {
		// Desktop: restore the recorded file. A folder flow's cross-subflow
		// indexes are rebuilt by the folder reload below. Snapshot the
		// CURRENT bytes first so the restore is itself undoable (parity with
		// the cloud branch).
		s.snapshotDesktopFile(doc, snap.TargetPath, "before restore")
		if err := atomicWriteConv(filepath.Dir(snap.TargetPath), snap.TargetPath, []byte(snap.source)); err != nil {
			return nil, fmt.Errorf("restore snapshot: %w", err)
		}
		// The restored bytes replaced the file: derived caches are stale.
		s.InvalidateSearchIndex(doc.ID)
		if doc.FilePath != "" {
			if info, serr := os.Stat(doc.FilePath); serr == nil && info.IsDir() {
				return s.LoadFlowFolder(doc.FilePath)
			}
			return s.LoadFlowFromPath(doc.FilePath)
		}
		// Snapshot recorded a file but the doc has no FilePath anymore
		// (switched docs?): restore the file and report via parse.
		restored, perr := parser.ParseText(snap.source, filepath.Base(snap.TargetPath), snap.Bytes)
		if perr != nil {
			return nil, fmt.Errorf("re-parse restored source: %w", perr)
		}
		return restored, nil
	}

	// Folder snapshot: re-parse the per-file map and persist the folder
	// through the same OCC path (a folder restored as a single combined text
	// would collapse the structure — the exact hazard SaveSource guards
	// against). The current state is snapshotted first, so the restore is
	// itself undoable.
	if snap.Files != nil {
		s.snapshotCloudFiles(doc, parser.SerializeFiles(doc), "before restore")
		combined := parseFilesPreservingIdentity(snap.Files, doc)
		if combined == nil {
			return nil, fmt.Errorf("restore: re-parse snapshot files failed")
		}
		updated, err := s.persistCloudDoc(ctx, doc, combined, "")
		if err != nil {
			return nil, err
		}
		logger.Info("folder snapshot restored", "flow", doc.ID, "snapshot", snapshotID, "label", snap.Label)
		return updated, nil
	}

	// Cloud: persist the old source through the standard path (OCC, re-parse,
	// SaveFlow) — and snapshot the CURRENT state first so the restore itself
	// is undoable.
	s.snapshotCloudSource(doc, "before restore")
	updated, err := s.persistCloudSource(ctx, doc, snap.source)
	if err != nil {
		return nil, err
	}
	logger.Info("source snapshot restored", "flow", doc.ID, "snapshot", snapshotID, "label", snap.Label)
	return updated, nil
}

// SourceMeta is the cheap change-detection signal for the desktop watcher:
// size + mtime of the flow's backing file (folder flows report the combined
// max mtime + total size across member files so ANY member edit registers).
type SourceMeta struct {
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
	// File count for folder flows (1 for single files); 0 when no local file.
	Files int `json:"files"`
}

// GetSourceMeta stats the flow's backing file(s) without reading content —
// the desktop watcher polls this. Cloud flows return Files=0 (nothing to
// watch; cloud sync arrives over the websocket flow-change channel instead).
func (s *FlowService) GetSourceMeta(doc *models.FlowDocument) (*SourceMeta, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	if doc.FilePath == "" {
		return &SourceMeta{Files: 0}, nil
	}
	info, err := os.Stat(doc.FilePath)
	if err != nil {
		// The watcher treats stat failure as "gone" (Files=0) rather than an
		// error: a file deleted between polls is a normal editor save dance.
		return &SourceMeta{Files: 0}, nil
	}
	if !info.IsDir() {
		return &SourceMeta{Size: info.Size(), ModTime: info.ModTime().UTC().Format(time.RFC3339Nano), Files: 1}, nil
	}
	// Folder: aggregate member files (the same *.txt/*.pad set loadFolder
	// consumes) so any member's edit moves the numbers.
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
