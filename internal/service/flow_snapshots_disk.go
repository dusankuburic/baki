package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// The desktop's undo ring on disk (R-approvals-c9). A desktop fix rewrites the user's own flow file in place; with
// the ring in memory only, closing the app, a crash or a sidecar respawn lost every way back. Each flow (keyed by
// the file or folder it restores to) has a directory under <config>/snapshots named by a hash of that path, holding
// one JSON file per snapshot. The bounds are the cloud ring's — maxFlowSnapshots entries and maxFlowSnapshotBytes per
// flow, storageif.FlowSnapshotTTL — plus maxDesktopSnapshotDirBytes for the whole directory, enforced at boot by
// dropping the oldest entries across flows.

// maxDesktopSnapshotDirBytes caps the snapshot directory as a whole.
const maxDesktopSnapshotDirBytes int64 = 256 << 20

// diskSnapshotFile is a snapshot as stored.
type diskSnapshotFile struct {
	ID         string            `json:"id"`
	Key        string            `json:"key"`
	FlowID     string            `json:"flowId"`
	Label      string            `json:"label"`
	CreatedAt  time.Time         `json:"createdAt"`
	Bytes      int64             `json:"bytes"`
	TargetPath string            `json:"targetPath"`
	Source     string            `json:"source,omitempty"`
	Files      map[string]string `json:"files,omitempty"`
}

func (f *diskSnapshotFile) snapshot() *SourceSnapshot {
	return &SourceSnapshot{ID: f.ID, FlowID: f.FlowID, Label: f.Label, CreatedAt: f.CreatedAt, Bytes: f.Bytes,
		TargetPath: f.TargetPath, Files: f.Files, source: f.Source}
}

// diskSnapshots is the on-disk ring; mu serializes its writes and trims within the process.
type diskSnapshots struct {
	dir      string
	maxTotal int64
	mu       sync.Mutex
}

func newDiskSnapshots(dir string, maxTotal int64) *diskSnapshots {
	return &diskSnapshots{dir: dir, maxTotal: maxTotal}
}

// WithDesktopSnapshotDir keeps the undo ring of desktop flows (a file or folder on disk) in dir, so it outlives the
// process; the directory is swept at construction. Without it the desktop ring is the memory one.
func WithDesktopSnapshotDir(dir string) FlowOption {
	return func(s *FlowService) {
		if dir == "" {
			return
		}
		s.diskSnaps = newDiskSnapshots(dir, maxDesktopSnapshotDirBytes)
		s.diskSnaps.sweep()
	}
}

// normKey is the ring's key for a flow path: cleaned, and case-folded where the file system ignores case (Windows),
// so the same file reopened as c:\flows\a.txt finds the ring recorded for C:\Flows\A.txt.
func normKey(key string) string {
	key = filepath.Clean(key)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}

// flowDir is key's directory: a hash, so no path component of the user's file reaches the file system here.
func (d *diskSnapshots) flowDir(key string) string {
	sum := sha256.Sum256([]byte(normKey(key)))
	return filepath.Join(d.dir, hex.EncodeToString(sum[:16]))
}

func (d *diskSnapshots) file(key, id string) string {
	return filepath.Join(d.flowDir(key), id+".json")
}

// save stores snap in key's ring (atomically; a snapshot already stored is left as it is) and trims the ring to its
// bounds. A snapshot larger than the flow's whole budget is not kept.
func (d *diskSnapshots) save(key string, snap *SourceSnapshot) error {
	if !safeConvComponent(snap.ID) {
		return fmt.Errorf("invalid snapshot id %q", snap.ID)
	}
	if snap.retainedBytes() > maxFlowSnapshotBytes {
		logger.Warn("source snapshot exceeds the undo budget; not kept", "flow", snap.FlowID, "bytes", snap.retainedBytes(), "budget", maxFlowSnapshotBytes)
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	path := d.file(key, snap.ID)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	data, err := json.Marshal(diskSnapshotFile{ID: snap.ID, Key: normKey(key), FlowID: snap.FlowID, Label: snap.Label, CreatedAt: snap.CreatedAt,
		Bytes: snap.Bytes, TargetPath: snap.TargetPath, Source: snap.source, Files: snap.Files})
	if err != nil {
		return err
	}
	dir := d.flowDir(key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := atomicWriteConv(dir, path, data); err != nil {
		return err
	}
	d.trimLocked(key)
	return nil
}

// remove drops one snapshot (a staged one whose write failed).
func (d *diskSnapshots) remove(key, id string) {
	if !safeConvComponent(id) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = os.Remove(d.file(key, id))
}

// list returns key's live snapshots, oldest first; an expired one is deleted on the way.
func (d *diskSnapshots) list(key string) []*SourceSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	recs := d.readLocked(d.flowDir(key), normKey(key))
	out := make([]*SourceSnapshot, len(recs))
	for i, r := range recs {
		out[i] = r.rec.snapshot()
	}
	return out
}

// take returns key's snapshot id.
func (d *diskSnapshots) take(key, id string) (*SourceSnapshot, bool) {
	for _, s := range d.list(key) {
		if s.ID == id {
			return s, true
		}
	}
	return nil, false
}

type diskRecord struct {
	path string
	size int64
	rec  *diskSnapshotFile
}

// readLocked reads dir's snapshots of key, oldest first, deleting the expired and the unreadable. key "" reads any.
func (d *diskSnapshots) readLocked(dir, key string) []diskRecord {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []diskRecord
	cutoff := time.Now().Add(-storageif.FlowSnapshotTTL)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path) // #nosec G304 -- a file of the snapshot directory
		if err != nil {
			continue
		}
		var rec diskSnapshotFile
		if json.Unmarshal(data, &rec) != nil || rec.ID == "" {
			_ = os.Remove(path)
			continue
		}
		if key != "" && rec.Key != key {
			continue // a hash collision: another flow's
		}
		if rec.CreatedAt.Before(cutoff) {
			_ = os.Remove(path)
			continue
		}
		out = append(out, diskRecord{path: path, size: int64(len(data)), rec: &rec})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].rec.CreatedAt.Equal(out[j].rec.CreatedAt) {
			return out[i].rec.CreatedAt.Before(out[j].rec.CreatedAt)
		}
		return out[i].rec.ID < out[j].rec.ID
	})
	return out
}

// trimLocked keeps key's newest maxFlowSnapshots entries within maxFlowSnapshotBytes (the newest is always kept).
func (d *diskSnapshots) trimLocked(key string) {
	recs := d.readLocked(d.flowDir(key), normKey(key))
	var total int64
	for _, r := range recs {
		total += r.rec.snapshot().retainedBytes()
	}
	for len(recs) > 1 && (len(recs) > maxFlowSnapshots || total > maxFlowSnapshotBytes) {
		total -= recs[0].rec.snapshot().retainedBytes()
		_ = os.Remove(recs[0].path)
		recs = recs[1:]
	}
}

// sweep is the boot pass: expired entries go, empty flow directories go, and the directory as a whole is brought
// under maxTotal by dropping the oldest entries across flows.
func (d *diskSnapshots) sweep() {
	d.mu.Lock()
	defer d.mu.Unlock()
	flows, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	var all []diskRecord
	var total int64
	for _, f := range flows {
		if !f.IsDir() {
			continue
		}
		dir := filepath.Join(d.dir, f.Name())
		recs := d.readLocked(dir, "")
		if len(recs) == 0 {
			_ = os.Remove(dir)
			continue
		}
		for _, r := range recs {
			total += r.rec.snapshot().retainedBytes()
		}
		all = append(all, recs...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].rec.CreatedAt.Before(all[j].rec.CreatedAt) })
	for len(all) > 0 && total > d.maxTotal {
		total -= all[0].rec.snapshot().retainedBytes()
		_ = os.Remove(all[0].path)
		all = all[1:]
	}
}

// snapshotTargetsOf checks that a desktop snapshot restores into doc: its target is the flow's file or folder, or a
// file directly in that folder (a folder flow's member fix), and a folder snapshot's member names stay inside it.
func snapshotTargetsOf(doc *models.FlowDocument, snap *SourceSnapshot) error {
	refuse := fmt.Errorf("%w: the snapshot does not belong to this flow's files", ErrInvalidInput)
	rel, err := filepath.Rel(filepath.Clean(doc.FilePath), filepath.Clean(snap.TargetPath))
	if err != nil || (rel != "." && (!filepath.IsLocal(rel) || strings.ContainsRune(rel, filepath.Separator))) {
		return refuse
	}
	for name := range snap.Files {
		if !filepath.IsLocal(name) {
			return refuse
		}
	}
	return nil
}

// desktopRing reports whether doc's undo ring is the on-disk one.
func (s *FlowService) desktopRing(doc *models.FlowDocument) bool {
	return s.diskSnaps != nil && doc != nil && doc.FilePath != ""
}
