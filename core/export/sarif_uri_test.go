package export

import (
	"encoding/json"
	"strings"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"
)

const uriFlow = "#Region \"Main\"\n" +
	"HTTPClient.InvokeUrl Method: GET Url: $'''https://api.example.com/x'''\n" +
	"#EndRegion\n"

// sarifURIs extracts every artifactLocation.uri from a generated SARIF doc.
func sarifURIs(t *testing.T, src, fileName string) []string {
	t.Helper()
	doc, err := parser.ParseText(src, fileName, int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	report := analyzer.RunAnalysis(doc, analyzer.AllRules(), models.DefaultSettings(), nil)
	out, err := ReportToSARIF(report, doc)
	if err != nil {
		t.Fatalf("ReportToSARIF: %v", err)
	}
	var parsed struct {
		Runs []struct {
			Results []struct {
				Locations []struct {
					PhysicalLocation struct {
						ArtifactLocation struct {
							URI string `json:"uri"`
						} `json:"artifactLocation"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("unmarshal SARIF: %v\n%s", err, out)
	}
	var uris []string
	for _, run := range parsed.Runs {
		for _, res := range run.Results {
			for _, loc := range res.Locations {
				uris = append(uris, loc.PhysicalLocation.ArtifactLocation.URI)
			}
		}
	}
	if len(uris) == 0 {
		t.Fatal("fixture produced no SARIF locations")
	}
	return uris
}

// TestSARIF_URIIsTheSourceFileNotARandomUUID is the regression gate for the
// defect that makes SARIF unusable on the single-file path.
//
// artifactURI reads doc.SubflowsByID[...].SourceFile, but buildDocument — the
// ParseText path used by `bakicli <file.txt>`, by stdin, and by the desktop
// single-flow open — never set it. So the fallback fired and returned
// f.SubflowID + ".txt", where SubflowID is a UUID the parser MINTS ON EVERY
// PARSE. GitHub Code Scanning resolves URIs against the repo, finds no such
// file, and the alert cannot be placed — and because the UUID is new each run,
// nothing correlates between runs either.
func TestSARIF_URIIsTheSourceFileNotARandomUUID(t *testing.T) {
	uris := sarifURIs(t, uriFlow, "Billing.txt")
	for _, uri := range uris {
		if uri != "Billing.txt" {
			t.Errorf("artifactLocation.uri = %q, want %q — a UUID-derived name matches no file in the repo",
				uri, "Billing.txt")
		}
	}
}

// TestSARIF_URIIsStableAcrossParses is the sharper half: two independent parses
// of identical text must produce identical URIs. A per-parse UUID passes the
// test above only if you squint (it is *a* name), but fails here — and this is
// what destroys run-to-run correlation in the Security tab.
func TestSARIF_URIIsStableAcrossParses(t *testing.T) {
	first := sarifURIs(t, uriFlow, "Billing.txt")
	second := sarifURIs(t, uriFlow, "Billing.txt")

	if len(first) != len(second) {
		t.Fatalf("finding count differs between parses: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("uri[%d] changed between two parses of identical text: %q then %q",
				i, first[i], second[i])
		}
	}
}

// TestSARIF_URINeverContainsAUUID guards the specific shape rather than just the
// happy path, so a future fallback cannot quietly reintroduce it.
func TestSARIF_URINeverContainsAUUID(t *testing.T) {
	for _, uri := range sarifURIs(t, uriFlow, "Billing.txt") {
		// A parser-minted UUID is 36 chars with dashes at fixed offsets.
		base := strings.TrimSuffix(uri, ".txt")
		if len(base) == 36 && strings.Count(base, "-") == 4 {
			t.Errorf("artifactLocation.uri looks like a UUID: %q", uri)
		}
	}
}
