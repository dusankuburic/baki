package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hostileName is tenant text that a chat payload must show as typed: a Slack mention of the whole channel, a Slack
// link, a Markdown link and an ampersand.
const hostileName = "<!channel> <https://evil.example|Reset password> [click](https://evil.example) R&D"

// capture runs n against a receiver and returns the body it got.
func capture(t *testing.T, n func(url string) AlertNotifier) []byte {
	t.Helper()
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- b
	}))
	t.Cleanup(srv.Close)
	ev := Event{Type: EventDrift, FlowID: "f1", FlowName: hostileName, Title: "New findings in " + hostileName,
		Message: "2 new finding(s) in " + hostileName, NewErrors: 1}
	if err := n(srv.URL).Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	return <-got
}

// Slack parses <…> control sequences (links, @channel mentions) in every text it shows, attachment fields included:
// a flow named "<!channel>" pinged the whole channel, and "<https://evil|Reset password>" became a link in a message
// the deployment's own channel receives for every tenant. Its three control characters are escaped, which Slack
// displays as typed.
func TestSlack_TenantTextCannotFormLinksOrMentions(t *testing.T) {
	body := capture(t, func(url string) AlertNotifier { return &SlackNotifier{URL: url, Client: http.DefaultClient} })
	var payload struct {
		Text        string `json:"text"`
		Attachments []struct {
			Text   string `json:"text"`
			Fields []struct {
				Value string `json:"value"`
			} `json:"fields"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	texts := []string{payload.Text, payload.Attachments[0].Text}
	for _, f := range payload.Attachments[0].Fields {
		texts = append(texts, f.Value)
	}
	for _, s := range texts {
		if strings.ContainsAny(s, "<>") {
			t.Errorf("Slack text %q keeps a control sequence", s)
		}
	}
	if want := "&lt;!channel&gt; &lt;https://evil.example|Reset password&gt; [click](https://evil.example) R&amp;D"; payload.Attachments[0].Fields[0].Value != want {
		t.Errorf("flow field = %q, want %q (Slack shows it as typed)", payload.Attachments[0].Fields[0].Value, want)
	}
}

// The connector card renders Markdown in a section's title, text and facts unless the section turns it off.
func TestTeams_TenantTextIsNotMarkdown(t *testing.T) {
	body := capture(t, func(url string) AlertNotifier { return &TeamsNotifier{URL: url, Client: http.DefaultClient} })
	var card struct {
		Sections []struct {
			Markdown *bool `json:"markdown"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatal(err)
	}
	for i, s := range card.Sections {
		if s.Markdown == nil || *s.Markdown {
			t.Errorf("section %d renders Markdown: a flow named %q shows a link", i, hostileName)
		}
	}
}
