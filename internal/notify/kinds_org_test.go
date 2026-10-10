package notify

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
)

// recordingMailer records every SendAlert and fails the addresses in fail.
type recordingMailer struct {
	mu       sync.Mutex
	to       []string
	subjects []string
	fail     map[string]bool
}

func (m *recordingMailer) SendAlert(_ context.Context, to, subject, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.to = append(m.to, to)
	m.subjects = append(m.subjects, subject)
	if m.fail[to] {
		return errors.New("relay refused")
	}
	return nil
}

func (m *recordingMailer) Enabled() bool { return true }

// TestEmailNotifier_OrgRecipientsEachGetTheMessage: an organisation's email channel sends the governance_alert
// message to each member address the service resolved, one message each (no member sees another's address), and one
// failed recipient does not keep the others from theirs.
func TestEmailNotifier_OrgRecipientsEachGetTheMessage(t *testing.T) {
	mailer := &recordingMailer{fail: map[string]bool{"b@acme.io": true}}
	n, err := Build(context.Background(), "email", ScopeOrg, Values{"recipients": "u-a,u-b,u-c"}, Deps{
		Mailer: mailer, MemberAddresses: []string{"a@acme.io", "b@acme.io", "c@acme.io"},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = n.Notify(context.Background(), sampleEvent())
	if err == nil {
		t.Error("a failed recipient was not reported")
	}
	if want := []string{"a@acme.io", "b@acme.io", "c@acme.io"}; !slices.Equal(mailer.to, want) {
		t.Errorf("sent to %v, want %v, one message each", mailer.to, want)
	}
	if mailer.subjects[0] == "" || mailer.subjects[0] != mailer.subjects[2] {
		t.Errorf("subjects %q: want the same governance_alert subject for every recipient", mailer.subjects)
	}
}

// TestEmailKind_OrgNeedsMembersAndMail: an organisation email channel whose recipients resolve to no member address
// is refused on recipients (skipped by the scanner, a 400 for the test), and the kind is unavailable without an
// enabled mail sender, as the deployment's is.
func TestEmailKind_OrgNeedsMembersAndMail(t *testing.T) {
	ctx := context.Background()
	_, err := Build(ctx, "email", ScopeOrg, Values{"recipients": "u-gone"}, Deps{Mailer: &recordingMailer{}})
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Field != "recipients" {
		t.Errorf("no member address: %v, want a recipients field error", err)
	}
	if _, err := Build(ctx, "email", ScopeOrg, Values{"recipients": "u-a"}, Deps{MemberAddresses: []string{"a@acme.io"}}); !errors.Is(err, ErrKindUnavailable) {
		t.Errorf("without a mailer: %v, want ErrKindUnavailable", err)
	}
	ids := make([]string, MaxEmailRecipients+1)
	for i := range ids {
		ids[i] = "u" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if err := Validate(ctx, "email", ScopeOrg, Values{"recipients": joinIDs(ids)}, BlockInternalTargets); !errors.As(err, &fe) || fe.Field != "recipients" {
		t.Errorf("%d recipients: %v, want refused", len(ids), err)
	}
	if got := PublicValues("email", Values{"recipients": "u-a,u-b"}); got["recipients"] != "u-a,u-b" || got["to"] != "" {
		t.Errorf("public values = %v", got)
	}
}

func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}

// TestJiraKind_PublicHidesTheToken: an organisation's Jira channel lists its URL redacted, the email and the project,
// never the API token.
func TestJiraKind_PublicHidesTheToken(t *testing.T) {
	got := PublicValues("jira", Values{"url": "https://acme.atlassian.net", "email": "bot@acme.io", "token": "XXXXTOKEN", "project": "PAD"})
	if got["token"] != "" || got["email"] != "bot@acme.io" || got["project"] != "PAD" || got["url"] != "acme.atlassian.net/…" {
		t.Errorf("public values = %v", got)
	}
}

// countingNotifier counts the events it was asked to deliver.
type countingNotifier struct{ got []EventType }

func (c *countingNotifier) Name() string { return "counting" }
func (c *countingNotifier) Notify(_ context.Context, ev Event) error {
	c.got = append(c.got, ev.Type)
	return nil
}

// TestSubscriptions: a subscription is one or more of the organisation events, stored in their order; a channel
// receives what it chose and the test event, nothing else; none chosen is the default set, today's.
func TestSubscriptions(t *testing.T) {
	got, err := ParseOrgEvents([]string{"new_rules", "drift", "drift"})
	if err != nil || !slices.Equal(got, []EventType{EventDrift, EventNewRules}) {
		t.Errorf("ParseOrgEvents = %v, %v; want [drift new_rules]", got, err)
	}
	for _, bad := range [][]string{nil, {}, {"test"}, {"analysis_complete"}, {"drift", "pager"}} {
		if _, err := ParseOrgEvents(bad); err == nil {
			t.Errorf("ParseOrgEvents(%q) accepted", bad)
		}
	}
	if !slices.Equal(SubscribedEvents(nil), []EventType{EventDrift, EventHealthRegression, EventNewRules}) {
		t.Errorf("default subscription = %v, want what organisation channels received before", SubscribedEvents(nil))
	}

	inner := &countingNotifier{}
	n := Subscribed(inner, []EventType{EventNewRules})
	for _, typ := range []EventType{EventDrift, EventHealthRegression, EventNewRules, EventTest} {
		if err := n.Notify(context.Background(), Event{Type: typ}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(inner.got, []EventType{EventNewRules, EventTest}) {
		t.Errorf("delivered %v, want the chosen event and the test", inner.got)
	}
	if Receives(n, EventDrift) || !Receives(n, EventNewRules) || !Receives(n, EventTest) || !Receives(inner, EventDrift) {
		t.Error("Receives disagrees with the subscription")
	}
	if n.Name() != "counting" {
		t.Errorf("Name = %q, want the channel's", n.Name())
	}
}

// TestJiraKind_OrgChecksLeaveTheDeploymentAlone: the project-key and account checks an organisation's Jira channel
// gets do not apply to PAD_NOTIFY_JIRA_*, which the previous release accepted as they are: refusing one now would
// fail the boot after the migration ran.
func TestJiraKind_OrgChecksLeaveTheDeploymentAlone(t *testing.T) {
	v := Values{"url": "https://203.0.113.10", "email": "svc:bot", "token": "t", "project": "My Project"}
	if err := Validate(context.Background(), "jira", ScopeDeployment, v, BlockInternalTargets); err != nil {
		t.Errorf("the deployment's Jira values: %v, want accepted as before", err)
	}
	var fe *FieldError
	if err := Validate(context.Background(), "jira", ScopeOrg, v, BlockInternalTargets); !errors.As(err, &fe) {
		t.Errorf("an organisation's: %v, want refused", err)
	}
}
