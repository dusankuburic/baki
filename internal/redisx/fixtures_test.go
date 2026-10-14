package redisx

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrevFixtures_EveryFileIsDecodedByATest: a captured payload nothing reads proves nothing. Every file under
// testdata/redis/prev/ must be named by a test of the package that decodes it (internal/**/*_test.go), so deleting
// the test, or adding a fixture and forgetting the test, fails here. The release process appends a directory per
// release (testdata/redis/prev/<release>/), captured from the tree of that release before any payload changes.
func TestPrevFixtures_EveryFileIsDecodedByATest(t *testing.T) {
	root := filepath.Join("testdata", "redis", "prev")
	var fixtures []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			fixtures = append(fixtures, filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no previous-release fixtures")
	}

	var tests strings.Builder
	err = filepath.WalkDir(filepath.Join("..", "..", "internal"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return fs.SkipDir
		}
		// The harness and this package only read the files; the decoders' own packages must decode them.
		slash := filepath.ToSlash(p)
		if !d.IsDir() && strings.HasSuffix(p, "_test.go") && !strings.Contains(slash, "/redisx/") && !strings.Contains(slash, "/redisreplica/") {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			tests.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		name := filepath.Base(f)
		release := filepath.Base(filepath.Dir(f))
		// A test names a fixture by its file name.
		if !strings.Contains(tests.String(), `"`+name+`"`) {
			t.Errorf("%s (release %s) is not decoded by any test: add one next to the decoder it covers", f, release)
		}
	}
}
