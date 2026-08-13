package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEmailSender records SendAlert calls and reports Enabled()=true.
type fakeEmailSender struct {
	lastTo      string
	lastSubject string
	lastPlain   string
	lastHTML    string
}

func (f *fakeEmailSender) SendAlert(_ context.Context, to, subject, plain, html string) error {
	f.lastTo, f.lastSubject, f.lastPlain, f.lastHTML = to, subject, plain, html
	return nil
}
func (f *fakeEmailSender) Enabled() bool { return true }

func TestEmailNotifier_RendersEventToSend(t *testing.T) {
	sender := &fakeEmailSender{}
	n := &EmailNotifier{Sender: sender, To: "ops@example.com"}
	ev := Event{
		Type:     EventAnalysisComplete,
		FlowName: "Payroll sync",
		Title:    "Analysis: Payroll sync",
		Message:  "3 error(s), 1 warning(s), 9 info finding(s)",
		Analysis: &AnalysisSummary{Errors: 3, Warnings: 1, Info: 9, HealthScore: 55},
	}
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if sender.lastTo != "ops@example.com" {
		t.Errorf("To = %q", sender.lastTo)
	}
	if !strings.Contains(sender.lastSubject, "ANALYSIS_COMPLETE") {
		t.Errorf("subject missing event type: %q", sender.lastSubject)
	}
	// Plain body should carry the counts and the flow name.
	for _, want := range []string{"Payroll sync", "Errors: 3", "Health: 55/100"} {
		if !strings.Contains(sender.lastPlain, want) {
			t.Errorf("plain body missing %q:\n%s", want, sender.lastPlain)
		}
	}
	// HTML body should escape nothing dangerous here but include the flow.
	if !strings.Contains(sender.lastHTML, "Payroll sync") {
		t.Errorf("html body missing flow name:\n%s", sender.lastHTML)
	}
}

func TestEmailNotifier_HTMLEscapesContent(t *testing.T) {
	sender := &fakeEmailSender{}
	n := &EmailNotifier{Sender: sender, To: "ops@example.com"}
	ev := Event{
		Type:     EventDrift,
		FlowName: "<script>x</script>",
		Title:    "t",
	}
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if strings.Contains(sender.lastHTML, "<script>") {
		t.Errorf("HTML body did not escape flow name:\n%s", sender.lastHTML)
	}
	if !strings.Contains(sender.lastHTML, "&lt;script&gt;") {
		t.Errorf("expected escaped flow name in HTML:\n%s", sender.lastHTML)
	}
}

func TestJiraNotifier_CreatesIssueWithBasicAuth(t *testing.T) {
	var gotAuth, gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"10001","key":"PAD-7"}`))
	}))
	defer srv.Close()

	n := &JiraNotifier{
		Base: srv.URL, Email: "bot@example.com", Token: "secrettoken", Project: "PAD",
		Client: srv.Client(),
	}
	ev := Event{Type: EventAnalysisComplete, FlowName: "Onboarding", Title: "Analysis: Onboarding",
		Analysis: &AnalysisSummary{Errors: 2, Warnings: 0, Info: 5}}
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	// POSTed to the issue-creation endpoint.
	if gotPath != "/rest/api/3/issue" {
		t.Errorf("path = %q, want /rest/api/3/issue", gotPath)
	}
	// Basic auth carries email:token.
	wantCred := base64.StdEncoding.EncodeToString([]byte("bot@example.com:secrettoken"))
	if gotAuth != "Basic "+wantCred {
		t.Errorf("Authorization = %q, want Basic %s", gotAuth, wantCred)
	}
	// Project + summary land in the right fields.
	fields, ok := body["fields"].(map[string]any)
	if !ok {
		t.Fatalf("missing fields object: %+v", body)
	}
	if proj, _ := fields["project"].(map[string]any); proj["key"] != "PAD" {
		t.Errorf("project key = %v", proj)
	}
	if fields["summary"] != "Analysis: Onboarding" {
		t.Errorf("summary = %v", fields["summary"])
	}
	// Errors present → Bug issue type.
	if it, _ := fields["issuetype"].(map[string]any); it["name"] != "Bug" {
		t.Errorf("issuetype = %v, want Bug for an event with errors", it["name"])
	}
}

func TestJiraNotifier_CleanEventIsTask(t *testing.T) {
	// No HTTP needed — jiraIssueType is a pure function.
	cases := []struct {
		ev   Event
		want string
	}{
		{Event{Type: EventAnalysisComplete, Analysis: &AnalysisSummary{Errors: 1}}, "Bug"},
		{Event{Type: EventAnalysisComplete, Analysis: &AnalysisSummary{Warnings: 4}}, "Task"},
		{Event{Type: EventHealthRegression, PrevHealth: 90, HealthScore: 40}, "Bug"},
		{Event{Type: EventDrift, NewErrors: 0, NewWarnings: 2}, "Task"},
	}
	for _, c := range cases {
		if got := jiraIssueType(c.ev); got != c.want {
			t.Errorf("jiraIssueType(%+v) = %q, want %q", c.ev, got, c.want)
		}
	}
}

func TestNew_IncludesEmailChannelWhenSenderEnabled(t *testing.T) {
	sender := &fakeEmailSender{}
	d, err := New(Config{EmailSender: sender, EmailTo: "ops@example.com"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !d.Enabled() {
		t.Fatal("expected dispatcher enabled with an email channel")
	}
	d.Dispatch(context.Background(), Event{Type: EventDrift, FlowID: "f1", Title: "x"})
	if sender.lastTo != "ops@example.com" {
		t.Errorf("email not delivered, To = %q", sender.lastTo)
	}
}

func TestNew_DisabledEmailWhenSenderNotEnabled(t *testing.T) {
	// A sender reporting Enabled()==false (e.g. log-only mailer) must NOT register
	// the email channel, so no spurious "delivered" events fire.
	d, err := New(Config{EmailSender: offSender{}, EmailTo: "ops@example.com"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.Enabled() {
		t.Fatal("email channel should be disabled when sender reports Enabled()==false")
	}
}

type offSender struct{}

func (offSender) SendAlert(context.Context, string, string, string, string) error { return nil }
func (offSender) Enabled() bool                                                   { return false }
