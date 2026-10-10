package analyzer

import (
	"context"
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// TestFlowHash_DistinguishesDocumentsThatOnlyDifferInParseErrors is the
// regression gate for a cross-caller disclosure through the shared analysis
// cache.
//
// FlowHash walked blocks only. A document whose every line fails to classify
// produces NO blocks, so all such documents hashed identically regardless of
// their source. StableFlowID completes the key, and for an upload it is
// sha256 of the FILE NAMES alone — content-independent by design. So two
// different callers uploading a "Main.txt" that does not parse collided on the
// entire cache key.
//
// The cache was process-global with no tenant or user in the key, so the
// first caller's report was returned to the second. That report is not a
// summary: a parse-error finding carries ParseError.Snippet, which is up to 200
// bytes of the other caller's VERBATIM SOURCE.
func TestFlowHash_DistinguishesDocumentsThatOnlyDifferInParseErrors(t *testing.T) {
	const secret = "PROD_DB_PASSWORD=hunter2-do-not-share"
	victim := "CASE '" + secret + "'\nCASE 'AKIAIOSFODNN7EXAMPLE'\n"
	attacker := "CASE 'nothing-of-mine'\n"

	vd, err := parser.ParseFiles(map[string]string{"Main.txt": victim}, "Main.txt")
	if err != nil {
		t.Fatalf("parse victim: %v", err)
	}
	ad, err := parser.ParseFiles(map[string]string{"Main.txt": attacker}, "Main.txt")
	if err != nil {
		t.Fatalf("parse attacker: %v", err)
	}

	// The premise: both produce zero blocks, and the identity component is
	// file-name derived, so FlowHash is the only thing that can separate them.
	if len(vd.ParseErrors) == 0 || len(ad.ParseErrors) == 0 {
		t.Fatal("fixture rot: these inputs no longer produce parse errors")
	}
	if StableFlowID(vd) != StableFlowID(ad) {
		t.Fatal("fixture rot: the identity component already differs, so this test would " +
			"pass without FlowHash distinguishing anything")
	}

	if FlowHash(vd) == FlowHash(ad) {
		t.Errorf("two documents with entirely different source share a FlowHash (%s) and a "+
			"StableFlowID (%s) — the full cache key. The cache is process-global with no "+
			"tenant in the key, so one caller's report is served to another",
			FlowHash(vd), StableFlowID(vd))
	}
}

// TestAnalysisCache_DoesNotServeAnotherCallersReport drives the property
// end-to-end through the cache itself, so the guarantee does not rest on
// FlowHash's internals staying the way they are today. Both callers are the
// same tenant here: the content alone must keep them apart.
func TestAnalysisCache_DoesNotServeAnotherCallersReport(t *testing.T) {
	const secret = "PROD_DB_PASSWORD=hunter2-do-not-share"
	victim := "CASE '" + secret + "'\n"
	attacker := "CASE 'nothing-of-mine'\n"

	vd, _ := parser.ParseFiles(map[string]string{"Main.txt": victim}, "Main.txt")
	ad, _ := parser.ParseFiles(map[string]string{"Main.txt": attacker}, "Main.txt")

	a := newTestAnalyzer()
	vr := a.Analyze(context.Background(), localIn(vd, AllRules(), models.DefaultSettings(), nil))

	// Sanity: the victim's report really does carry their source verbatim, or
	// this test is asserting nothing.
	var carries bool
	for _, f := range vr.Findings {
		if snip, ok := f.Metadata["snippet"].(string); ok && strings.Contains(snip, secret) {
			carries = true
		}
	}
	if !carries {
		t.Fatal("fixture rot: the victim's report no longer embeds their source, so a " +
			"cache hit would not disclose anything")
	}

	if got := a.Lookup(localIn(ad, AllRules(), models.DefaultSettings(), nil)); got != nil {
		for _, f := range got.Findings {
			if snip, ok := f.Metadata["snippet"].(string); ok && strings.Contains(snip, secret) {
				t.Fatalf("the attacker's lookup returned the victim's report, disclosing %q", snip)
			}
		}
		t.Error("the attacker's lookup hit the victim's cache entry")
	}
}

// TestFlowHash_StableAcrossReparses keeps the cache working: the whole point of
// hashing structure rather than identity is that byte-identical source
// re-parses to the same key. Adding parse errors to the hash must not break it.
func TestFlowHash_StableAcrossReparses(t *testing.T) {
	for _, src := range []string{
		"#Region \"Main\"\nHTTPClient.InvokeUrl Method: GET Url: 'https://x/y'\n#EndRegion\n",
		"CASE 'unparseable'\nELSE\n",
	} {
		a, _ := parser.ParseFiles(map[string]string{"Main.txt": src}, "Main.txt")
		b, _ := parser.ParseFiles(map[string]string{"Main.txt": src}, "Main.txt")
		if FlowHash(a) != FlowHash(b) {
			t.Errorf("identical source hashed differently across parses (%s vs %s) — "+
				"the analysis cache can never hit", FlowHash(a), FlowHash(b))
		}
	}
}
