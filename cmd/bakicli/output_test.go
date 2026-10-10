package main

import (
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-core/export"
)

// parsesAs fails the test unless stdout is one well-formed document of format.
func parsesAs(t *testing.T, format, stdout string) {
	t.Helper()
	var err error
	switch format {
	case "json", "sarif":
		dec := json.NewDecoder(strings.NewReader(stdout))
		var v any
		if err = dec.Decode(&v); err == nil && dec.More() {
			err = errTrailing
		}
	case "junit":
		var v struct {
			XMLName xml.Name `xml:"testsuites"`
		}
		err = xml.Unmarshal([]byte(stdout), &v)
		if err == nil && !strings.HasSuffix(strings.TrimRight(stdout, "\n"), "</testsuites>") {
			err = errTrailing
		}
	case "csv":
		r := csv.NewReader(strings.NewReader(stdout))
		r.Comment = '#'
		_, err = r.ReadAll()
	default:
		t.Fatalf("no parser for %s", format)
	}
	if err != nil {
		t.Errorf("-format %s: stdout does not parse: %v\n%s", format, err, stdout)
	}
}

var errTrailing = errors.New("trailing data after the document")

// TestCLI_StructuredStdoutStaysParseable pins the D1 fix: with a structured
// -format, the ratchets' status lines go to stderr. The CI templates redirect
// stdout to the SARIF they upload; "baseline drift: …" after the document made
// it invalid whenever baseline or new-from-rev was set.
func TestCLI_StructuredStdoutStaysParseable(t *testing.T) {
	dir := t.TempDir()
	flow := filepath.Join(dir, "flow.txt")
	if err := os.WriteFile(flow, []byte(cliTestFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "baseline.json")
	if err := os.WriteFile(existing, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
		info string
	}{
		{"baseline", []string{"-baseline", existing}, "baseline drift:"},
		{"missing baseline", []string{"-baseline", filepath.Join(dir, "absent.json")}, "no baseline at"},
		{"update-baseline", []string{"-update-baseline", filepath.Join(dir, "written.json")}, "baseline written:"},
	}
	for _, format := range []string{"sarif", "json", "junit", "csv"} {
		for _, c := range cases {
			args := append(append([]string{"-config", "", "-format", format}, c.args...), flow)
			stdout, stderr, code := runBakicli(t, args...)
			if code == 2 {
				t.Fatalf("%s/%s: exit 2: %s", format, c.name, stderr)
			}
			parsesAs(t, format, stdout)
			if strings.Contains(stdout, c.info) || !strings.Contains(stderr, c.info) {
				t.Errorf("%s/%s: %q must be on stderr only\nstdout=%s\nstderr=%s", format, c.name, c.info, stdout, stderr)
			}
		}
	}

	// Text output is unchanged: the lines stay on stdout after the report.
	stdout, _, _ := runBakicli(t, "-config", "", "-baseline", existing, flow)
	if !strings.Contains(stdout, "baseline drift: 1 new (1 errors, 0 warnings, 0 info) out of 1 total\n") {
		t.Errorf("text output lost its baseline line:\n%s", stdout)
	}
}

// TestCLI_NewFromRevKeepsSARIFValid: the same for -new-from-rev, and a bad
// revision exits 2 before anything is written (exit 2 means "no report").
func TestCLI_NewFromRevKeepsSARIFValid(t *testing.T) {
	root := newGitRepo(t, map[string]string{"flows/Main.txt": cliCleanFlow})
	if err := os.WriteFile(filepath.Join(root, "flows", "Main.txt"), []byte(cliTestFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runBakicliIn(t, root, nil, "-config", "", "-format", "sarif", "-new-from-rev", "HEAD", "flows")
	if code != 1 {
		t.Fatalf("a new error since HEAD: exit %d, want 1 (stderr=%s)", code, stderr)
	}
	parsesAs(t, "sarif", stdout)
	if !strings.Contains(stderr, "new since HEAD: 1 (1 errors") {
		t.Errorf("stderr lacks the new-since line: %s", stderr)
	}

	stdout, stderr, code = runBakicliIn(t, root, nil, "-config", "", "-format", "sarif", "-new-from-rev", "no-such-branch", "flows")
	if code != 2 || stdout != "" {
		t.Errorf("bad revision: exit %d stdout %d bytes, want 2 and nothing (stderr=%s)", code, len(stdout), stderr)
	}
}

// TestCLI_SARIFCarriesTheCLIVersion pins the D2 fix: tool.driver.version is
// the binary's version ("dev" without ldflags), not the library default.
func TestCLI_SARIFCarriesTheCLIVersion(t *testing.T) {
	stdout, _, _ := runBakicli(t, "-config", "", "-format", "sarif", writeTempFlow(t, cliTestFlow))
	var sarif struct {
		Runs []struct {
			Tool struct {
				Driver struct {
					Version string `json:"version"`
				} `json:"driver"`
			} `json:"tool"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(stdout), &sarif); err != nil {
		t.Fatal(err)
	}
	if got := sarif.Runs[0].Tool.Driver.Version; got != "dev" {
		t.Errorf("driver.version = %q, want the CLI's own version %q", got, "dev")
	}
}

// TestCLI_EveryRegisteredFormat: -format offers every export format (markdown
// and pdf included) in registry order, and each writes its document.
func TestCLI_EveryRegisteredFormat(t *testing.T) {
	flow := writeTempFlow(t, cliTestFlow)
	starts := map[string]string{
		"json": "{", "sarif": "{", "junit": "<?xml", "csv": "# PAD Analyzer", "html": "<!DOCTYPE html>",
		"markdown": "# PAD Analyzer", "pdf": "%PDF-",
	}
	for _, id := range export.IDs() {
		want, ok := starts[id]
		if !ok {
			t.Errorf("format %q has no case here; add one", id)
			continue
		}
		stdout, stderr, code := runBakicli(t, "-config", "", "-format", id, flow)
		if code != 1 || !strings.HasPrefix(stdout, want) {
			t.Errorf("-format %s: exit %d, stdout starts %q, want exit 1 and %q (stderr=%s)",
				id, code, stdout[:min(40, len(stdout))], want, stderr)
		}
	}

	stdout, stderr, _ := runBakicli(t, "-h")
	if want := "Output formats (-format): text, json, sarif, junit, csv, html, markdown, pdf\n"; !strings.Contains(stdout+stderr, want) {
		t.Errorf("help lacks %q", want)
	}
	_, stderr, code := runBakicli(t, "-format", "xml", flow)
	if code != 2 || !strings.Contains(stderr, "must be text, json, sarif, junit, csv, html, markdown, or pdf") {
		t.Errorf("unknown format: exit %d stderr=%s", code, stderr)
	}
}

// TestCLI_FormatJSONEqualsReportJSON: -format json and -report-json are the
// same registry format over the same run, so the bytes are equal.
func TestCLI_FormatJSONEqualsReportJSON(t *testing.T) {
	side := filepath.Join(t.TempDir(), "report.json")
	stdout, _, _ := runBakicli(t, "-config", "", "-format", "json", "-report-json", side, writeTempFlow(t, cliTestFlow))
	data, err := os.ReadFile(side)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != stdout {
		t.Errorf("-report-json differs from -format json:\n%s\n---\n%s", data, stdout)
	}
}

func TestCheckOutput(t *testing.T) {
	cases := []struct {
		format   string
		targets  []string
		terminal bool
		wantErr  string
	}{
		{"pdf", []string{"a.txt"}, true, "binary"},
		{"pdf", []string{"a.txt"}, false, ""},
		{"text", []string{"a.txt"}, true, ""},
		{"sarif", []string{"a.txt"}, true, ""},
		{"sarif", []string{"a.txt", "b.txt"}, false, "renders one flow"},
		{"pdf", []string{"a.txt", "b.txt"}, false, "renders one flow"},
		{"json", []string{"a.txt", "b.txt"}, false, ""},
		{"text", []string{"a.txt", "b.txt"}, false, ""},
		{"text", []string{"a.txt", "-"}, false, "stdin"},
		{"text", []string{"-"}, false, ""},
	}
	for _, c := range cases {
		err := checkOutput(c.format, c.targets, c.terminal)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s %v terminal=%v: unexpected %v", c.format, c.targets, c.terminal, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s %v terminal=%v: err %v, want %q", c.format, c.targets, c.terminal, err, c.wantErr)
		}
	}
}

// TestIsTerminal: files, the null device and pipes are not terminals (the
// positive case needs a console and is not testable here).
func TestIsTerminal(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for name, file := range map[string]*os.File{"file": f, "null": null, "pipe": w} {
		if isTerminal(file) {
			t.Errorf("%s reported as a terminal", name)
		}
	}
}

// TestCLI_FlagAfterAPathExits2: flag parsing stops at the first path, so a
// trailing flag was read as a second flow ("parse error: ... -format"); it is
// a usage error that names the flag, and nothing is written.
func TestCLI_FlagAfterAPathExits2(t *testing.T) {
	flow := writeTempFlow(t, cliTestFlow)
	stdout, stderr, code := runBakicli(t, "-config", "", flow, "-format", "json")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "flags must come before the paths: -format") {
		t.Errorf("exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
