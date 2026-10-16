package audit

import (
	"os"
	"path/filepath"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// seedSpill writes raw as the spill file of instance "test" in a fresh directory and opens the store over it, as a
// boot after a crash does.
func seedSpill(t *testing.T, raw string) *spillStore {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, spillFileFor("test")), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newSpillStore(dir, "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A writer that died mid-append leaves a last line without its newline. The next spill must start on a line of its
// own: appended to the fragment, the new event became part of one undecodable line and was lost with it.
func TestSpill_TornLastLineDoesNotSwallowTheNextEvent(t *testing.T) {
	s := seedSpill(t, `{"ID":"a","Action":"test"}`+"\n"+`{"ID":"b","Act`)
	before := auditSpillUndecodableCount()
	if !s.Spill(ev("c")) {
		t.Fatal("Spill(c) refused")
	}
	if got := ids(s.Drain(10)); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("Drain = %v, want [a c]: the torn b is lost, c must not be", got)
	}
	if n := auditSpillUndecodableCount() - before; n != 1 {
		t.Errorf("pad_audit_spill_undecodable_total rose by %v, want 1 (the torn line)", n)
	}
	if s.HasItems() {
		t.Error("the file still holds something after the drain")
	}
}

// The replay's re-append (reSpill) gets the same guard.
func TestSpill_ReSpillAfterATornLineStartsANewLine(t *testing.T) {
	s := seedSpill(t, `{"ID":"torn`)
	s.reSpill([]*storageif.AuditEvent{ev("r")})
	if got := ids(s.Drain(10)); len(got) != 1 || got[0] != "r" {
		t.Fatalf("Drain = %v, want [r]", got)
	}
}

// An undecodable line is skipped and counted, and the lines after it still replay; the warning is logged once per
// file, not once per line.
func TestSpill_DrainSkipsAndCountsUndecodableLines(t *testing.T) {
	s := seedSpill(t, `{"ID":"a","Action":"test"}`+"\nnot json\n"+`{"ID":"b","Action":"test"}`+"\n{broken\n")
	before := auditSpillUndecodableCount()
	if got := ids(s.Drain(10)); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Drain = %v, want [a b]", got)
	}
	if n := auditSpillUndecodableCount() - before; n != 2 {
		t.Errorf("pad_audit_spill_undecodable_total rose by %v, want 2", n)
	}
	if !s.warnedUndecodable {
		t.Error("the first undecodable line was not logged")
	}
}
