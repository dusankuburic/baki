package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// writeSpillFile writes the events as a spill file named name in dir, aged by age (its mtime is now - age).
func writeSpillFile(t *testing.T, dir, name string, age time.Duration, ids ...string) string {
	t.Helper()
	var sb strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&sb, "{\"ID\":%q,\"Action\":\"test\"}\n", id)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustSpill(t *testing.T, dir, instance string) *spillStore {
	t.Helper()
	s, err := newSpillStore(dir, instance, 0)
	if err != nil {
		t.Fatalf("newSpillStore(%q): %v", instance, err)
	}
	return s
}

func drainAll(s *spillStore) []string {
	var out []string
	for {
		b := s.Drain(10)
		if len(b) == 0 {
			return out
		}
		out = append(out, ids(b)...)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// Each replica spills to a file of its own, so two processes on one volume never rewrite each other's file.
func TestSpill_PerInstanceFile(t *testing.T) {
	dir := t.TempDir()
	a, b := mustSpill(t, dir, "replica-a"), mustSpill(t, dir, "replica-b")
	a.Spill(ev("a1"))
	b.Spill(ev("b1"))
	a.Spill(ev("a2"))

	if got := drainAll(b); len(got) != 1 || got[0] != "b1" {
		t.Fatalf("b drained %v, want [b1]", got)
	}
	if got := drainAll(a); len(got) != 2 || got[0] != "a1" || got[1] != "a2" {
		t.Fatalf("a drained %v, want [a1 a2]", got)
	}
	for _, want := range []string{"audit-spill-replica-a.jsonl", "audit-spill-replica-b.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("expected %s: %v", want, err)
		}
	}
}

// The instance id is a file name component: anything outside [A-Za-z0-9._-] cannot reach the path.
func TestSpill_InstanceNameIsSanitised(t *testing.T) {
	for in, want := range map[string]string{
		"app--0000001-abc12": "audit-spill-app--0000001-abc12.jsonl",
		"host name/with:bad": "audit-spill-host_name_with_bad.jsonl",
		"../../etc/passwd":   "audit-spill-.._.._etc_passwd.jsonl",
		"":                   "audit-spill-unnamed.jsonl",
	} {
		if got := spillFileFor(in); got != want {
			t.Errorf("spillFileFor(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("x", 500)
	if got := spillFileFor(long); len(got) > 120 {
		t.Errorf("a long instance id gave a %d-byte file name", len(got))
	}
}

// pad_audit_spill_depth under-reported a file left by a previous boot until it drained: the depth starts from
// what the file holds.
func TestSpill_DepthStartsFromTheExistingFile(t *testing.T) {
	dir := t.TempDir()
	writeSpillFile(t, dir, spillFileFor("me"), 0, "a", "b", "c")
	s := mustSpill(t, dir, "me")
	if s.depth != 3 {
		t.Fatalf("depth at open = %d, want 3", s.depth)
	}
	if got := auditSpillDepthCount(); got != 3 {
		t.Errorf("pad_audit_spill_depth at open = %v, want 3", got)
	}
	s.Drain(1)
	if got := auditSpillDepthCount(); got != 2 {
		t.Errorf("pad_audit_spill_depth after a drain = %v, want 2", got)
	}
}

// A corrupt line is skipped by Drain; it must not leave the depth above what the file holds.
func TestSpill_DepthIgnoresCorruptLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, spillFileFor("me"))
	if err := os.WriteFile(p, []byte("{\"ID\":\"a\"}\nnot json\n{\"ID\":\"b\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := mustSpill(t, dir, "me")
	if got := s.Drain(10); len(got) != 2 {
		t.Fatalf("drained %d, want 2", len(got))
	}
	if s.depth != 0 {
		t.Errorf("depth after emptying the file = %d, want 0", s.depth)
	}
}

// A file of a replica that is gone (stale) is claimed, once, and its events survive in the claimant's hands.
func TestAdoptOrphans_ClaimsAStaleFile(t *testing.T) {
	dir := t.TempDir()
	writeSpillFile(t, dir, "audit-spill-dead.jsonl", 10*time.Minute, "x", "y")
	got := claimOrphans(dir, "me", 2*time.Minute)
	if len(got) != 1 {
		t.Fatalf("claimed %v, want one file", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit-spill-dead.jsonl")); !os.IsNotExist(err) {
		t.Errorf("the orphan must be renamed away, stat err = %v", err)
	}
	s, err := openAdopted(got[0], nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ids := drainAll(s); len(ids) != 2 || ids[0] != "x" || ids[1] != "y" {
		t.Errorf("adopted events = %v, want [x y]", ids)
	}
	if again := claimOrphans(dir, "me", 2*time.Minute); len(again) != 0 {
		t.Errorf("a second scan claimed %v, want nothing (our own claim is not an orphan)", again)
	}
}

// Liveness: a file that was written to recently, our own file and files that are not spill files are never claimed.
func TestAdoptOrphans_LeavesLiveAndForeignFilesAlone(t *testing.T) {
	dir := t.TempDir()
	writeSpillFile(t, dir, "audit-spill-live.jsonl", 5*time.Second, "x")
	writeSpillFile(t, dir, spillFileFor("me"), time.Hour, "mine")
	writeSpillFile(t, dir, "notes.jsonl", time.Hour, "n")
	writeSpillFile(t, dir, "audit-spill-other.txt", time.Hour, "n")
	before := dirNames(t, dir)
	if got := claimOrphans(dir, "me", 2*time.Minute); len(got) != 0 {
		t.Fatalf("claimed %v, want nothing", got)
	}
	if after := dirNames(t, dir); strings.Join(before, ",") != strings.Join(after, ",") {
		t.Errorf("directory changed: %v -> %v", before, after)
	}
}

// The file the release before wrote (a fixed name) is adopted like any other orphan.
func TestAdoptOrphans_ClaimsTheLegacyFile(t *testing.T) {
	dir := t.TempDir()
	writeSpillFile(t, dir, "audit-spill.jsonl", time.Hour, "old1")
	got := claimOrphans(dir, "me", 2*time.Minute)
	if len(got) != 1 {
		t.Fatalf("claimed %v, want the legacy file", got)
	}
	s, _ := openAdopted(got[0], nil, 0)
	if ids := drainAll(s); len(ids) != 1 || ids[0] != "old1" {
		t.Errorf("legacy events = %v", ids)
	}
}

// A claim left by a replica that died while replaying it is an orphan too; one of ours (a restart under the same
// instance id) is picked up at once.
func TestAdoptOrphans_ClaimsLeftoversOfClaims(t *testing.T) {
	dir := t.TempDir()
	writeSpillFile(t, dir, "audit-spill-dead.jsonl.adopt-gone-1700000000000000000", time.Hour, "g")
	writeSpillFile(t, dir, "audit-spill-dead.jsonl.adopt-me-1700000000000000001", 0, "m")
	own := ownClaims(dir, "me")
	got := claimOrphans(dir, "me", 2*time.Minute)
	if len(own) != 1 || len(got) != 1 {
		t.Fatalf("own claims %v, claimed orphans %v, want one each", own, got)
	}
	var all []string
	for _, p := range append(own, got...) {
		s, _ := openAdopted(p, nil, 0)
		all = append(all, drainAll(s)...)
	}
	if len(all) != 2 {
		t.Errorf("events = %v, want g and m", all)
	}
}

// The scratch file of a drain that died, and the marker of a claim that died, are garbage once stale.
func TestClaimOrphans_RemovesStaleDrainScratch(t *testing.T) {
	dir := t.TempDir()
	writeSpillFile(t, dir, "audit-spill-dead.jsonl.drain.tmp", time.Hour, "x")
	writeSpillFile(t, dir, "audit-spill-live.jsonl.drain.tmp", time.Second, "x")
	writeSpillFile(t, dir, "audit-spill-gone.jsonl.claiming", time.Hour)
	claimOrphans(dir, "me", 2*time.Minute)
	if names := dirNames(t, dir); len(names) != 1 || names[0] != "audit-spill-live.jsonl.drain.tmp" {
		t.Errorf("left %v, want only the fresh scratch file", names)
	}
}

// Empty files are no work; a live replica's file is empty most of the time, so only an old one is removed.
func TestAdoptOrphans_EmptyFiles(t *testing.T) {
	dir := t.TempDir()
	writeSpillFile(t, dir, "audit-spill-idle.jsonl", time.Hour)
	writeSpillFile(t, dir, "audit-spill-ancient.jsonl", 48*time.Hour)
	if got := claimOrphans(dir, "me", 2*time.Minute); len(got) != 0 {
		t.Fatalf("claimed %v, want no work", got)
	}
	names := dirNames(t, dir)
	if len(names) != 1 || names[0] != "audit-spill-idle.jsonl" {
		t.Errorf("left %v, want only the idle file (the ancient empty one is removed)", names)
	}
}

// Eight replicas start at once on a shared volume: the orphan is claimed by exactly one, and its events replayed once.
func TestAdoptOrphans_RacingAdoptersClaimOnce(t *testing.T) {
	dir := t.TempDir()
	writeSpillFile(t, dir, "audit-spill-dead.jsonl", time.Hour, "1", "2", "3", "4")
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed []string
		start   = make(chan struct{})
	)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got := claimOrphans(dir, fmt.Sprintf("replica-%d", i), 2*time.Minute)
			mu.Lock()
			claimed = append(claimed, got...)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if len(claimed) != 1 {
		t.Fatalf("%d adopters claimed the file (%v), want exactly one", len(claimed), claimed)
	}
	s, _ := openAdopted(claimed[0], nil, 0)
	if ids := drainAll(s); len(ids) != 4 {
		t.Errorf("replayed %v, want the 4 events once", ids)
	}
}

// End to end: a recorder started over a directory holding a dead replica's file stores its events, and an event
// the store has already (the replay of a file after a crash between the insert and the file rewrite) adds no row.
func TestRecorder_ReplaysAdoptedAndAlreadyStoredEvents(t *testing.T) {
	dir := t.TempDir()
	store := testutil.NewFakeBackend()
	if err := store.SaveAuditEvent(t.Context(), &storageif.AuditEvent{ID: "stored", Action: "test"}); err != nil {
		t.Fatal(err)
	}
	writeSpillFile(t, dir, "audit-spill-dead.jsonl", time.Hour, "stored", "new1", "new2")
	writeSpillFile(t, dir, spillFileFor("me"), 0, "own1", "stored")

	r := New(store, Config{SpillDir: dir, Instance: "me"})
	r.Start()
	t.Cleanup(r.Stop)

	deadline := time.Now().Add(5 * time.Second)
	for len(store.SavedAuditEvents()) < 4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	r.Stop()
	got := map[string]int{}
	for _, e := range store.SavedAuditEvents() {
		got[e.ID]++
	}
	for _, id := range []string{"stored", "new1", "new2", "own1"} {
		if got[id] != 1 {
			t.Errorf("event %q stored %d times, want once (all: %v)", id, got[id], got)
		}
	}
	for _, n := range dirNames(t, dir) {
		if strings.Contains(n, ".adopt-") {
			t.Errorf("a drained claim must be deleted, found %s", n)
		}
	}
}

// pad_audit_spill_depth counts our own file and the claims we replay.
func TestRecorder_DepthCountsAdoptedFiles(t *testing.T) {
	dir := t.TempDir()
	writeSpillFile(t, dir, spillFileFor("me"), 0, "own1")
	writeSpillFile(t, dir, "audit-spill-dead.jsonl", time.Hour, "a", "b")
	r := New(testutil.NewFakeBackend(), Config{SpillDir: dir, Instance: "me"})
	r.adoptOrphans()
	if got := auditSpillDepthCount(); got != 3 {
		t.Errorf("pad_audit_spill_depth = %v, want 3 (own 1 + adopted 2)", got)
	}
}
