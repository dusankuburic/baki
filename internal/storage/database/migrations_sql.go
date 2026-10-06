package database

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// sqlMigrationFS holds the schema steps from v32 on (see migrations/README.md); v1–v31 stay frozen as Go literals.
//
//go:embed migrations
var sqlMigrationFS embed.FS

// migrationFileRE matches NNNN_name.up.sql / NNNN_name.down.sql.
var migrationFileRE = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.(up|down)\.sql$`)

// compatDirective declares a step's compatibility in its .up.sql file, on a line of its own.
const compatDirective = "-- baki:compat "

// mustLoadSQLMigrations loads the embedded steps and checks they continue the Go list without a gap. A malformed
// set panics at init: a binary must never boot with a step silently skipped (TestMigrations_SQLFilesLoad
// catches it in CI first).
func mustLoadSQLMigrations(after int) []migration {
	steps, err := loadSQLMigrations(sqlMigrationFS, "migrations")
	if err == nil {
		err = checkMigrationSequence(steps, after)
	}
	if err != nil {
		panic(err)
	}
	return steps
}

// checkMigrationSequence requires steps to continue version after without gaps.
func checkMigrationSequence(steps []migration, after int) error {
	for i, m := range steps {
		if want := after + 1 + i; m.version != want {
			return fmt.Errorf("migrations: %04d_%s is out of sequence: want version %d next (versions continue the Go list without gaps)", m.version, m.name, want)
		}
	}
	return nil
}

// loadSQLMigrations parses the step files in dir, ordered by version. Up files must declare their compat; a down
// file is optional (absent = irreversible) but must match its up file's name.
func loadSQLMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("migrations: read %s: %w", dir, err)
	}
	type files struct {
		name     string
		up, down *string
	}
	byVersion := map[int]*files{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue // README.md and friends
		}
		parts := migrationFileRE.FindStringSubmatch(name)
		if parts == nil {
			return nil, fmt.Errorf("migrations: %s: want NNNN_name.up.sql or NNNN_name.down.sql (lowercase name, digits and _)", name)
		}
		version, _ := strconv.Atoi(parts[1])
		raw, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("migrations: read %s: %w", name, err)
		}
		text := normalizeMigrationSQL(raw)
		f := byVersion[version]
		if f == nil {
			f = &files{name: parts[2]}
			byVersion[version] = f
		}
		if f.name != parts[2] {
			return nil, fmt.Errorf("migrations: version %d has files named %q and %q", version, f.name, parts[2])
		}
		slot := &f.up
		if parts[3] == "down" {
			slot = &f.down
		}
		if *slot != nil {
			return nil, fmt.Errorf("migrations: duplicate %s file for version %d", parts[3], version)
		}
		*slot = &text
	}

	versions := make([]int, 0, len(byVersion))
	for v := range byVersion {
		versions = append(versions, v)
	}
	sort.Ints(versions)
	out := make([]migration, 0, len(versions))
	for _, v := range versions {
		f := byVersion[v]
		if f.up == nil {
			return nil, fmt.Errorf("migrations: version %d (%s) has a down file but no up file", v, f.name)
		}
		compat, err := parseCompat(*f.up)
		if err != nil {
			return nil, fmt.Errorf("migrations: %04d_%s.up.sql: %w", v, f.name, err)
		}
		m := migration{version: v, name: f.name, sql: *f.up, compat: compat}
		if f.down != nil {
			m.downSQL = *f.down
		}
		out = append(out, m)
	}
	return out, nil
}

// normalizeMigrationSQL makes a step's text identical on every checkout (no UTF-8 BOM, LF line endings) before it
// is hashed, so a Windows CRLF checkout cannot look like an edited migration.
func normalizeMigrationSQL(raw []byte) string {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// parseCompat reads the step's single "-- baki:compat expand|breaking" line.
func parseCompat(sqlText string) (migrationCompat, error) {
	var found []migrationCompat
	for _, line := range strings.Split(sqlText, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, compatDirective) {
			continue
		}
		switch c := migrationCompat(strings.TrimSpace(strings.TrimPrefix(line, compatDirective))); c {
		case compatExpand, compatBreaking:
			found = append(found, c)
		default:
			return "", fmt.Errorf("unknown compat %q (want %q or %q)", c, compatExpand, compatBreaking)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("want exactly one %q line (expand or breaking), found %d", strings.TrimSpace(compatDirective), len(found))
	}
	return found[0], nil
}
