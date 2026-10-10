package mail

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update-mail-golden", false, "rewrite internal/mail/testdata/*.golden from the registered templates")

// The golden fixtures: one per registered template (variant "") plus the branches worth pinning. User-controlled
// fields carry markup, quotes and a forged MIME boundary on purpose.
const (
	goldenBase = "https://pad.example.com"
	goldenFrom = "PAD Analyzer <no-reply@pad.example.com>"
	goldenTo   = "una@example.com"
	hostileOrg = `Contoso & "Partners" <EU>`
	hostileFlw = `Invoices & Payments "EU" <v2>`
	hostileCmt = "Looks risky, see <b>step 4</b> & retry +1.\n--pad_mime_boundary_8a3f\nContent-Type: text/html; charset=UTF-8\n\n<a href=\"https://evil.example\">Reset your password</a>"
)

var goldenAt = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

type fixture struct {
	id, variant string
	base        string
	data        any
}

func (f fixture) name() string {
	if f.variant == "" {
		return f.id
	}
	return f.id + "-" + f.variant
}

var fixtures = []fixture{
	{"password_reset", "", goldenBase, PasswordResetData{Token: "tok_RESET-123"}},
	{"password_reset", "no_base_url", "", PasswordResetData{Token: "tok_RESET-123"}},
	{"email_verification", "", goldenBase, EmailVerificationData{Token: "tok_VERIFY-456"}},
	{"org_invite", "", goldenBase, OrgInviteData{OrgName: hostileOrg, Token: "tok_INVITE-789"}},
	{"finding_assigned", "", goldenBase, FindingAssignedData{AssigneeName: "Una O'Neil", AssignerName: "Ari <admin>", FlowName: hostileFlw, FindingTitle: "Unused variable"}},
	{"finding_assigned", "no_base_url", "", FindingAssignedData{AssigneeName: "Una O'Neil", AssignerName: "Ari <admin>", FlowName: hostileFlw, FindingTitle: "Unused variable"}},
	{"finding_comment", "", goldenBase, FindingCommentData{RecipientName: "Una O'Neil", CommenterName: "Ari <admin>", FlowName: hostileFlw, FindingTitle: "Unused variable", Comment: hostileCmt}},
	{"finding_comment", "no_flow_access", goldenBase, FindingCommentData{RecipientName: "Una O'Neil", CommenterName: "Ari <admin>", FindingTitle: "Unused variable", Comment: "please look"}},
	{"governance_alert", "", "", GovernanceAlertData{Event: "drift", FlowID: "flow-1", FlowName: hostileFlw, Title: "New findings in " + hostileFlw, Message: "2 new errors <since> the last scan & 1 warning", NewErrors: 2, NewWarnings: 1, At: goldenAt}},
	{"governance_alert", "health_regression", "", GovernanceAlertData{Event: "health_regression", FlowID: "flow-1", Title: "Health dropped", Message: "Health fell below the threshold", HealthScore: 61, PrevHealth: 88, At: goldenAt}},
	{"governance_alert", "analysis_complete", "", GovernanceAlertData{Event: "analysis_complete", FlowName: "Invoices", Title: "Analysis complete: Invoices", Analysis: &GovernanceAlertAnalysis{Errors: 3, Warnings: 5, Info: 7, HealthScore: 72}, At: goldenAt}},
	{"governance_alert", "test", "", GovernanceAlertData{Event: "test", Title: "Test notification", Message: "This is a test from PAD Analyzer.", At: goldenAt}},
}

func renderFixture(t *testing.T, f fixture) Message {
	t.Helper()
	e, ok := lookup(f.id)
	if !ok {
		t.Fatalf("fixture %s: no registered template %q", f.name(), f.id)
	}
	msg, err := e.renderAny(f.data, Env{BaseURL: f.base}, "")
	if err != nil {
		t.Fatalf("fixture %s: %v", f.name(), err)
	}
	return msg
}

// goldenEML is the full message with the per-message randomness pinned, its CRLF line ends written as LF
// (.gitattributes keeps *.golden LF; TestBuildMessage_CRLFOnly pins the real line ends).
func goldenEML(t *testing.T, msg Message) string {
	t.Helper()
	from, err := parseAddress(goldenFrom)
	if err != nil {
		t.Fatal(err)
	}
	to, err := parseAddress(goldenTo)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := buildMessage(from, to, msg, mimeOpts{now: goldenAt, boundary: "GOLDEN-BOUNDARY", messageID: "<golden@pad.example.com>"})
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// TestMailGolden pins every rendered email (subject, text, HTML, and the whole MIME message) byte for byte.
// Refresh with: go test ./internal/mail/ -run TestMailGolden -update-mail-golden, and review the diff.
func TestMailGolden(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.name(), func(t *testing.T) {
			msg := renderFixture(t, f)
			for ext, got := range map[string]string{
				"subject": msg.Subject + "\n",
				"txt":     msg.Text,
				"html":    msg.HTML,
				"eml":     goldenEML(t, msg),
			} {
				path := filepath.Join("testdata", f.name()+"."+ext+".golden")
				if *updateGolden {
					if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%v (run with -update-mail-golden)", err)
				}
				if got != strings.ReplaceAll(string(want), "\r", "") { // a CRLF checkout passes
					t.Errorf("%s differs from the golden:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
				}
			}
		})
	}
}

// TestMailGolden_Coverage: every registered template has a fixture and its goldens, and every golden file belongs to
// a fixture (a removed template takes its goldens with it).
func TestMailGolden_Coverage(t *testing.T) {
	main := map[string]bool{}
	names := map[string]bool{}
	for _, f := range fixtures {
		if f.variant == "" {
			main[f.id] = true
		}
		names[f.name()] = true
	}
	for _, id := range IDs() {
		if !main[id] {
			t.Errorf("template %q has no fixture: add one to fixtures (variant \"\") and run -update-mail-golden", id)
			continue
		}
		for _, ext := range []string{"subject", "txt", "html", "eml"} {
			if _, err := os.Stat(filepath.Join("testdata", id+"."+ext+".golden")); err != nil {
				t.Errorf("template %q: %v", id, err)
			}
		}
	}
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range files {
		base := filepath.Base(p)
		name := base[:strings.Index(base, ".")]
		if !names[name] {
			t.Errorf("%s belongs to no fixture", p)
		}
	}
	for _, f := range fixtures {
		if _, ok := lookup(f.id); !ok {
			t.Errorf("fixture %s names no registered template", f.name())
		}
	}
}
