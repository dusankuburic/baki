package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMtimesChanged(t *testing.T) {
	a := map[string]time.Time{"f1": {}, "f2": {}}
	b := map[string]time.Time{"f1": {}, "f2": {}}
	if mtimesChanged(a, b) {
		t.Error("identical maps should not be 'changed'")
	}
	// Different mtime on one file → changed.
	b["f1"] = time.Unix(1000, 0)
	if !mtimesChanged(a, b) {
		t.Error("a changed mtime must be detected")
	}
	// Different file set → changed.
	c := map[string]time.Time{"f1": {}, "f3": {}}
	if !mtimesChanged(a, c) {
		t.Error("a changed file set must be detected")
	}
}

func TestSnapshotMt_DetectsMissingAndChanged(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f1, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap1 := snapshotMt([]string{f1})
	mt1 := snap1[f1]
	if mt1.IsZero() {
		t.Fatal("snapshot should record a non-zero mtime for an existing file")
	}

	// Missing file → zero mtime (so a delete+recreate is detected as a change).
	missing := filepath.Join(dir, "nope.txt")
	snapMissing := snapshotMt([]string{missing})
	if !snapMissing[missing].IsZero() {
		t.Error("missing file should snapshot to zero time")
	}

	// Bump mtime into the future; snapshot must reflect the new value.
	future := mt1.Add(2 * time.Second)
	if err := os.Chtimes(f1, future, future); err != nil {
		t.Fatal(err)
	}
	snap2 := snapshotMt([]string{f1})
	if snap2[f1].Equal(mt1) {
		t.Error("snapshot after Chtimes should differ from the prior mtime")
	}
	if mtimesChanged(snap1, snap2) == false {
		t.Error("a changed mtime must register as changed")
	}
}
