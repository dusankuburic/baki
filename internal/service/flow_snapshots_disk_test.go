package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// R-approvals-c9: a desktop flow's undo ring is kept on disk under the config directory, so a "before fix" snapshot
// of an AI-applied fix outlives the process that recorded it.

const desktopFixSource = "#Region \"Main\"\n    HTTPClient.InvokeUrl Url: $'''https://target''' Method: HTTPClient.Method.GET\n#EndRegion\n"

// desktopSnapshotService is a desktop FlowService whose undo ring lives in dir.
func desktopSnapshotService(t *testing.T, dir string) *FlowService {
	t.Helper()
	return NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, nil, WithDesktopSnapshotDir(dir))
}

func writeDesktopFlow(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Main.txt")
	if err := os.WriteFile(path, []byte(desktopFixSource), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDesktopSnapshots_OutliveTheProcess: a fix applied through the desktop path leaves a "before fix" snapshot that
// a new service on the same directory (the backend restarted) lists, and restoring it brings back the exact bytes.
func TestDesktopSnapshots_OutliveTheProcess(t *testing.T) {
	snapDir := t.TempDir()
	path := writeDesktopFlow(t)
	first := desktopSnapshotService(t, snapDir)
	doc, err := first.LoadFlowFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ApplyFix(context.Background(), doc, httpURLBlockIDRecursive(doc, "https://target"), "wrap-error-handler", "unhandled-error", "", "", false); err != nil {
		t.Fatalf("ApplyFix: %v", err)
	}
	if fixed, _ := os.ReadFile(path); string(fixed) == desktopFixSource {
		t.Fatal("the fix did not change the file")
	}

	restarted := desktopSnapshotService(t, snapDir)
	doc, err = restarted.LoadFlowFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := restarted.ListSourceSnapshots(context.Background(), doc)
	if err != nil || len(snaps) != 1 || snaps[0].Label != "before fix" {
		t.Fatalf("snapshots after restart = %+v (%v), want the one before fix", snaps, err)
	}
	if _, err := restarted.RestoreSourceSnapshot(context.Background(), doc, snaps[0].ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != desktopFixSource {
		t.Errorf("restored bytes:\n%s\nwant the pre-fix source", got)
	}
	// The restore is itself undoable, and that entry is on disk too.
	if snaps, _ := desktopSnapshotService(t, snapDir).ListSourceSnapshots(context.Background(), doc); len(snaps) != 2 || snaps[1].Label != "before restore" {
		t.Errorf("snapshots after the restore = %+v, want the restore's own entry last", snaps)
	}
}

// TestDesktopSnapshots_RestoreRefusesAMissingTarget: a snapshot whose file was deleted is not restored (nothing
// to restore into; writing it would create a file the user removed).
func TestDesktopSnapshots_RestoreRefusesAMissingTarget(t *testing.T) {
	snapDir := t.TempDir()
	path := writeDesktopFlow(t)
	svc := desktopSnapshotService(t, snapDir)
	doc, err := svc.LoadFlowFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyFix(context.Background(), doc, httpURLBlockIDRecursive(doc, "https://target"), "wrap-error-handler", "unhandled-error", "", "", false); err != nil {
		t.Fatalf("ApplyFix: %v", err)
	}
	snaps, _ := svc.ListSourceSnapshots(context.Background(), doc)
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %+v", snaps)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err = svc.RestoreSourceSnapshot(context.Background(), doc, snaps[0].ID)
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("restore of a deleted target: %v, want a clear refusal", err)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Error("the refused restore re-created the deleted file")
	}
}

// TestDesktopSnapshots_Bounds: the ring keeps the cloud ring's bounds — 20 entries and 32 MiB per flow, 24 hours —
// and a service starting on the directory prunes what expired and keeps the whole directory under its byte cap,
// dropping the oldest entries first.
func TestDesktopSnapshots_Bounds(t *testing.T) {
	snapDir := t.TempDir()
	svc := desktopSnapshotService(t, snapDir)
	doc := &models.FlowDocument{ID: "d1", FilePath: filepath.Join(t.TempDir(), "Main.txt")}
	for i := 0; i < 25; i++ {
		svc.pushSnapshot(context.Background(), doc, &SourceSnapshot{ID: fmt.Sprintf("s%02d", i), FlowID: doc.ID, Label: "before fix",
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond), Bytes: 4, TargetPath: doc.FilePath, source: "text"})
	}
	snaps, _ := svc.ListSourceSnapshots(context.Background(), doc)
	if len(snaps) != maxFlowSnapshots || snaps[0].ID != "s05" || snaps[len(snaps)-1].ID != "s24" {
		t.Fatalf("ring = %d entries (%s..%s), want the newest %d", len(snaps), snaps[0].ID, snaps[len(snaps)-1].ID, maxFlowSnapshots)
	}

	// Per-flow bytes: one entry over the flow's whole budget is not kept; entries that together pass it drop the
	// oldest.
	big := strings.Repeat("x", int(maxFlowSnapshotBytes/3))
	for i := 0; i < 4; i++ {
		svc.pushSnapshot(context.Background(), doc, &SourceSnapshot{ID: fmt.Sprintf("big%d", i), FlowID: doc.ID, Label: "before fix",
			CreatedAt: time.Now().UTC().Add(time.Second + time.Duration(i)*time.Millisecond), Bytes: int64(len(big)), TargetPath: doc.FilePath, source: big})
	}
	snaps, _ = svc.ListSourceSnapshots(context.Background(), doc)
	var total int64
	for _, s := range snaps {
		total += s.Bytes
	}
	if total > maxFlowSnapshotBytes || snaps[len(snaps)-1].ID != "big3" {
		t.Errorf("ring holds %d bytes ending %s, want at most %d with the newest kept", total, snaps[len(snaps)-1].ID, maxFlowSnapshotBytes)
	}

	// An entry past the TTL is pruned by the next service starting on the directory.
	other := &models.FlowDocument{ID: "d2", FilePath: filepath.Join(t.TempDir(), "Other.txt")}
	svc.pushSnapshot(context.Background(), other, &SourceSnapshot{ID: "old", FlowID: other.ID, Label: "before fix",
		CreatedAt: time.Now().UTC().Add(-25 * time.Hour), Bytes: 4, TargetPath: other.FilePath, source: "text"})
	if snaps, _ := desktopSnapshotService(t, snapDir).ListSourceSnapshots(context.Background(), other); len(snaps) != 0 {
		t.Errorf("an expired snapshot survived the boot sweep: %+v", snaps)
	}
}

// TestDesktopSnapshots_BootSweepEnforcesTheDirectoryCap: the directory as a whole stays under its cap, the oldest
// entries across flows going first.
func TestDesktopSnapshots_BootSweepEnforcesTheDirectoryCap(t *testing.T) {
	snapDir := t.TempDir()
	ds := newDiskSnapshots(snapDir, 10)
	for i := 0; i < 4; i++ {
		key := filepath.Join(t.TempDir(), fmt.Sprintf("F%d.txt", i))
		snap := &SourceSnapshot{ID: fmt.Sprintf("s%d", i), Label: "before fix", CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
			Bytes: 4, TargetPath: key, source: "text"}
		if err := ds.save(key, snap); err != nil {
			t.Fatal(err)
		}
	}
	ds.sweep()
	var left []string
	_ = filepath.WalkDir(snapDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			var rec struct {
				ID string `json:"id"`
			}
			if data, rerr := os.ReadFile(p); rerr == nil && json.Unmarshal(data, &rec) == nil {
				left = append(left, rec.ID)
			}
		}
		return nil
	})
	if len(left) != 2 || !strings.Contains(strings.Join(left, ","), "s3") || !strings.Contains(strings.Join(left, ","), "s2") {
		t.Errorf("after the sweep %v remain, want the two newest (s2, s3) under a 10-byte cap", left)
	}
}

// TestDesktopSnapshots_RestoreChecksTheRecordedPaths: the snapshot files are read back from disk, so a restore takes
// nothing on trust — the target must be the flow's own file or a file of its folder, and a folder member's name must
// stay inside the folder.
func TestDesktopSnapshots_RestoreChecksTheRecordedPaths(t *testing.T) {
	snapDir := t.TempDir()
	svc := desktopSnapshotService(t, snapDir)
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "Main.txt"), []byte(desktopFixSource), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.txt")
	if err := os.WriteFile(outside, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := &models.FlowDocument{ID: "f1", FilePath: folder}
	key := flowSnapshotKey(doc)
	for _, snap := range []*SourceSnapshot{
		{ID: "other-file", Label: "before fix", CreatedAt: time.Now().UTC(), Bytes: 4, TargetPath: outside, source: "evil"},
		{ID: "escaping-member", Label: "before fix", CreatedAt: time.Now().UTC(), Bytes: 4, TargetPath: folder,
			Files: map[string]string{"../elsewhere.txt": "evil"}},
	} {
		if err := svc.diskSnaps.save(key, snap); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.RestoreSourceSnapshot(context.Background(), doc, snap.ID); err == nil {
			t.Errorf("snapshot %s was restored", snap.ID)
		}
	}
	if got, _ := os.ReadFile(outside); string(got) != "keep me" {
		t.Errorf("a file outside the flow was written: %q", got)
	}
}

// TestDesktopSnapshots_SameFileAnotherSpelling: on Windows the file system ignores case, so the same file opened
// under another spelling of its path finds the ring recorded for it.
func TestDesktopSnapshots_SameFileAnotherSpelling(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("path case matters on this file system")
	}
	svc := desktopSnapshotService(t, t.TempDir())
	path := filepath.Join(t.TempDir(), "Main.txt")
	recorded := &models.FlowDocument{ID: "f1", FilePath: path}
	svc.pushSnapshot(context.Background(), recorded, &SourceSnapshot{ID: "s1", Label: "before fix", CreatedAt: time.Now().UTC(), Bytes: 4, TargetPath: path, source: "text"})
	reopened := &models.FlowDocument{ID: "f1", FilePath: strings.ToUpper(path[:1]) + strings.ToLower(path[1:])}
	if snaps, _ := svc.ListSourceSnapshots(context.Background(), reopened); len(snaps) != 1 {
		t.Errorf("the ring recorded for %s was not found as %s", path, reopened.FilePath)
	}
}
