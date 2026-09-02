package analyzer

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// TestSerializeRoundTrip_FindingsStable is the analysis half of the
// serializer gate (the tree half lives in parser/serialize_test.go — an
// import cycle prevents combining them): every sample flow's findings, by
// (ruleId, severity, title) multiset, are IDENTICAL after
// parse → serialize → re-parse. This is what makes serialization safe as
// the fix/export pipeline's source of truth: a fix computed against the
// serialized source targets the same findings the user saw.
func TestSerializeRoundTrip_FindingsStable(t *testing.T) {
	var samples []string
	root := "../parser/testdata/samples"
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && (strings.HasSuffix(p, ".txt") || strings.HasSuffix(p, ".pad")) {
			samples = append(samples, p)
		}
		return err
	})
	if err != nil || len(samples) < 10 {
		t.Fatalf("samples walk: %v (%d files)", err, len(samples))
	}

	// Parse-error findings are EXCLUDED: canonical serialization heals
	// malformed input lines by design (their raw form isn't retained), so
	// round-tripping can only DECREASE parse errors — never increase them
	// (asserted separately). Every semantic finding must survive unchanged.
	findingKeys := func(doc *models.FlowDocument) []string {
		report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
		ks := make([]string, 0, len(report.Findings))
		for _, f := range report.Findings {
			if f.RuleID == "parse-error" {
				continue
			}
			ks = append(ks, f.RuleID+"|"+string(f.Severity)+"|"+f.Title)
		}
		sort.Strings(ks)
		return ks
	}

	parseErrCount := func(doc *models.FlowDocument) int {
		n := 0
		for _, e := range doc.ParseErrors {
			if e.Severity == "error" {
				n++
			}
		}
		return n
	}

	for _, path := range samples {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path) // #nosec G304 -- test corpus
			if err != nil {
				t.Fatal(err)
			}
			orig, err := parser.ParseText(string(data), filepath.Base(path), int64(len(data)))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			out := parser.SerializeDocument(orig)
			round, err := parser.ParseText(out, "round", int64(len(out)))
			if err != nil {
				t.Fatalf("round parse: %v\n--- serialized ---\n%s", err, out)
			}
			a, b := findingKeys(orig), findingKeys(round)
			if strings.Join(a, ";") != strings.Join(b, ";") {
				t.Fatalf("semantic findings changed:\norig:  %v\nround: %v\n--- serialized ---\n%s", a, b, out)
			}
			if parseErrCount(round) > parseErrCount(orig) {
				t.Fatalf("round-trip introduced parse errors (%d → %d)\n--- serialized ---\n%s",
					parseErrCount(orig), parseErrCount(round), out)
			}
		})
	}
}
