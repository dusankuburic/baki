package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ldflagXPattern matches one `-X <import path>.<Symbol>=<value>` entry.
var ldflagXPattern = regexp.MustCompile(`-X\s+([\w./-]+)\.(\w+)=`)

// TestDockerfileLdflagsTargetRealSymbols verifies that every symbol the image
// build stamps actually exists.
//
// `go build -X pkg.Sym=value` is SILENT when pkg.Sym does not exist — no error,
// no warning, just a value that never lands. The image build had drifted
// exactly that way: /api/system/info reads Version, GitCommit and BuildDate
// from internal/service, but only internal/service.Version was stamped, so the
// endpoint operators use to identify a running deployment permanently answered
// gitCommit="" and buildDate="". Nothing failed; the field was just always
// empty.
//
// This test does not assert WHICH symbols are stamped — that is a build
// decision — only that each one names a variable that exists, so a typo or a
// moved/renamed variable is caught at test time instead of shipping a
// permanently blank field.
func TestDockerfileLdflagsTargetRealSymbols(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	matches := ldflagXPattern.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		t.Fatal("no -X ldflags found in the Dockerfile — the pattern or the build changed")
	}

	for _, m := range matches {
		pkgPath, symbol := m[1], m[2]
		t.Run(pkgPath+"."+symbol, func(t *testing.T) {
			dir, err := packageDir(pkgPath)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if !declaresVar(t, dir, symbol) {
				t.Errorf("Dockerfile stamps -X %s.%s= but no such package-level variable exists in %s/ "+
					"— the linker silently ignores this, so the value would never appear at runtime",
					pkgPath, symbol, dir)
			}
		})
	}
}

// packageDir maps an ldflags import path to its directory in this module.
func packageDir(pkgPath string) (string, error) {
	const module = "pad-analyzer/"
	switch {
	case pkgPath == "main":
		return ".", nil
	case strings.HasPrefix(pkgPath, module):
		return strings.TrimPrefix(pkgPath, module), nil
	}
	return "", &unknownPkgError{pkgPath}
}

type unknownPkgError struct{ path string }

func (e *unknownPkgError) Error() string {
	return "ldflags reference package " + e.path + ", which is outside this module — it cannot be stamped from this build"
}

// declaresVar reports whether any non-test .go file in dir declares symbol as a
// package-level variable, in either `var X = ...` or a `var (...)` block.
func declaresVar(t *testing.T, dir, symbol string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	// `var Sym =`, or `Sym  = ...` on its own line inside a var block.
	decl := regexp.MustCompile(`(?m)^\s*(?:var\s+)?` + regexp.QuoteMeta(symbol) + `\s+[\w.\[\]*]*\s*=`)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if decl.Match(src) {
			return true
		}
	}
	return false
}
