package notify

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var updateNotifyGolden = flag.Bool("update-notify-golden", false, "rewrite testdata/payloads/*.golden")

// goldenAt is the fixed time of every golden event.
var goldenAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// goldenEvents is one event of each type a channel can receive, with the ids the scanner and the services give
// them.
func goldenEvents() []Event {
	return []Event{
		{ID: EventID("flow-1|drift|e1w1"), Type: EventDrift, FlowID: "flow-1", FlowName: "Invoice Bot", Title: "New findings in Invoice Bot",
			Message: "2 new finding(s) since baseline (1 error, 1 warning).", NewErrors: 1, NewWarnings: 1, At: goldenAt},
		{ID: EventID("flow-1|health_regression|h70<90"), Type: EventHealthRegression, FlowID: "flow-1", FlowName: "Invoice Bot", Title: "Health regressed in Invoice Bot",
			Message: "Health score dropped from 90 to 70.", HealthScore: 70, PrevHealth: 90, At: goldenAt},
		{ID: "evt_0123456789abcdef01234567", Type: EventAnalysisComplete, FlowName: "Invoice Bot", Title: "PAD Flow Analysis: Invoice Bot (errors)",
			Message: "1 error(s), 2 warning(s), 3 info finding(s)", Analysis: &AnalysisSummary{Errors: 1, Warnings: 2, Info: 3, HealthScore: 81}, At: goldenAt},
		{ID: "evt_76543210fedcba9876543210", Type: EventTest, Title: "baki channel test", Message: `This is a test delivery to "ops" (webhook). If you can read this, the channel works.`, At: goldenAt},
		{ID: EventID("new_rules|org|org-1|digest"), Type: EventNewRules, Title: "2 new rules report findings", Message: "Rules newer than the flows' baselines report findings in 3 flow(s): rule-a, rule-b.",
			RuleIDs: []string{"rule-a", "rule-b"}, FlowCount: 3, At: goldenAt},
	}
}

// goldenEnvelope stamps every golden delivery alike: attempts one second after the event, delivery ids numbered.
func goldenEnvelope() Envelope {
	n := 0
	return Envelope{
		Now: func() time.Time { return goldenAt.Add(time.Second) },
		NewID: func() string {
			n++
			return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
		},
		UserAgent: UserAgent("test"),
	}
}

// goldenKinds lists every kind with a payload.
var goldenKinds = []string{"webhook", "slack", "teams", "teams-workflow", "jira", "email"}

// recordedRequest is what a receiver saw.
type recordedRequest struct {
	method, path string
	header       http.Header
	body         []byte
}

// TestPayloadGolden pins the bytes and headers each kind sends for each event type, so a change of what receivers
// get is a reviewed diff of testdata/payloads. Refresh with -update-notify-golden.
func TestPayloadGolden(t *testing.T) {
	type golden struct {
		name, kind, orgID string
		ev                Event
	}
	var cases []golden
	for _, kind := range goldenKinds {
		for _, ev := range goldenEvents() {
			orgID := ""
			// Rendered as an organisation's channel. The email kind renders the deployment's: an organisation's sends
			// the same message to each chosen member (TestEmailNotifier_OrgRecipientsEachGetTheMessage).
			if spec, _ := Kind(kind); spec.Scope&ScopeOrg != 0 && kind != "email" {
				orgID = "org-1"
			}
			cases = append(cases, golden{kind + "." + string(ev.Type), kind, orgID, ev})
		}
	}
	// The deployment's webhook: no orgId.
	cases = append(cases, golden{"webhook.drift.deployment", "webhook", "", goldenEvents()[0]})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderGolden(t, c.kind, c.orgID, c.ev)
			path := filepath.Join("testdata", "payloads", c.name+".golden")
			if *updateNotifyGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update-notify-golden)", err)
			}
			if strings.ReplaceAll(string(want), "\r", "") != got {
				t.Errorf("%s changed (run with -update-notify-golden and review the diff):\n--- want\n%s\n--- got\n%s", path, want, got)
			}
		})
	}
}

// renderGolden delivers ev through kind, built from the registry with the golden envelope, and renders what the
// receiver saw.
func renderGolden(t *testing.T, kind, orgID string, ev Event) string {
	t.Helper()
	var mu sync.Mutex
	var got []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, recordedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	values := map[string]Values{
		"webhook":        {"url": srv.URL + "/hook", "secret": "golden-secret"},
		"slack":          {"url": srv.URL + "/services/T/B/X", "secret": "golden-secret"},
		"teams":          {"url": srv.URL + "/webhookb2/x"},
		"teams-workflow": {"url": srv.URL + "/workflows/x/triggers/manual/paths/invoke?api-version=1&sig=golden"},
		"jira":           {"url": srv.URL, "email": "bot@example.com", "token": "jira-token", "project": "PAD"},
		"email":          {"to": "ops@example.com"},
	}[kind]
	scope := ScopeDeployment
	if orgID != "" {
		scope = ScopeOrg
	}
	sender := &fakeEmailSender{}
	n, err := Build(context.Background(), kind, scope, values, Deps{
		Client: srv.Client(), Policy: AllowLoopbackTargets, Mailer: sender, OrgID: orgID, Envelope: goldenEnvelope(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if kind == "email" {
		return fmt.Sprintf("To: %s\nSubject: %s\n\n--- text\n%s\n--- html\n%s\n", sender.lastTo, sender.lastSubject, sender.lastPlain, sender.lastHTML)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("%d requests, want 1", len(got))
	}
	return renderRequest(got[0])
}

// renderRequest prints a request as its method and path, its headers sorted (without the transport's own), a blank
// line and the body bytes.
func renderRequest(r recordedRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", r.method, r.path)
	names := make([]string, 0, len(r.header))
	for name := range r.header {
		switch name {
		case "Accept-Encoding", "Content-Length":
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(&b, "%s: %s\n", name, strings.Join(r.header[name], ", "))
	}
	b.WriteString("\n")
	b.Write(r.body)
	b.WriteString("\n")
	return b.String()
}
