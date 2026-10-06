package database

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateMigrationChecksums = flag.Bool("update-migration-checksums", false,
	"rewrite testdata/migration_checksums.golden (only after ADDING a migration)")

// TestMigrationChecksums_Golden pins every shipped step's checksum. Boot refuses a database whose recorded checksum
// no longer matches ("drift"), which used to surface only at deploy time; this catches an edited shipped step in
// CI instead. After adding a step, refresh the golden with -update-migration-checksums.
func TestMigrationChecksums_Golden(t *testing.T) {
	var b strings.Builder
	for _, m := range migrations {
		fmt.Fprintf(&b, "%d %s %s\n", m.version, m.name, migrationChecksum(m.sql))
	}
	got := b.String()
	path := filepath.Join("testdata", "migration_checksums.golden")
	if *updateMigrationChecksums {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (create it with -update-migration-checksums)", err)
	}
	want := strings.ReplaceAll(string(raw), "\r\n", "\n") // a CRLF checkout of the golden is not a mismatch
	if got == want {
		return
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
		if gotLines[i] != wantLines[i] {
			t.Fatalf("migration checksum mismatch at line %d:\n  golden: %s\n  now:    %s\n"+
				"A shipped migration's SQL changed; boot would refuse every existing database. Revert the edit and "+
				"append a new step instead. (Only when you just ADDED a step: rerun with -update-migration-checksums.)",
				i+1, wantLines[i], gotLines[i])
		}
	}
	t.Fatalf("migration list length changed (golden %d lines, now %d); after ADDING a step rerun with -update-migration-checksums",
		len(wantLines), len(gotLines))
}
