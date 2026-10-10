package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postTeamsWorkflow delivers ev through a teams-workflow channel built from the registry and returns the decoded
// body the receiver saw.
func postTeamsWorkflow(t *testing.T, ev Event) map[string]any {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted) // what a Workflows trigger answers
	}))
	defer srv.Close()
	n, err := Build(context.Background(), "teams-workflow", ScopeOrg, Values{"url": srv.URL + "/workflows/x"},
		Deps{Client: srv.Client(), Policy: AllowLoopbackTargets})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	return msg
}

// TestTeamsWorkflow_PostsAnAdaptiveCardMessage: a Workflows webhook takes a message whose attachment is an Adaptive
// Card (the "Post to a channel when a webhook request is received" template posts each attachment's content).
func TestTeamsWorkflow_PostsAnAdaptiveCardMessage(t *testing.T) {
	msg := postTeamsWorkflow(t, sampleEvent())
	if msg["type"] != "message" {
		t.Errorf("type = %v, want message", msg["type"])
	}
	atts, _ := msg["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("attachments = %v, want one", msg["attachments"])
	}
	att := atts[0].(map[string]any)
	if att["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("contentType = %v", att["contentType"])
	}
	if v, ok := att["contentUrl"]; !ok || v != nil {
		t.Errorf("contentUrl = %v (present %v), want null", v, ok)
	}
	card := att["content"].(map[string]any)
	if card["type"] != "AdaptiveCard" || card["version"] != "1.4" || card["$schema"] != "http://adaptivecards.io/schemas/adaptive-card.json" {
		t.Errorf("card header = %v %v %v", card["type"], card["version"], card["$schema"])
	}
	text := strings.Join(cardTexts(card), "\n")
	for _, want := range []string{"New findings in Invoice Bot", "2 new findings since baseline", "Flow: ", "Invoice Bot", "New findings: ", "1 error(s), 1 warning(s)"} {
		if !strings.Contains(text, want) {
			t.Errorf("the card does not say %q:\n%s", want, text)
		}
	}
}

// TestTeamsWorkflow_TenantTextIsNeverMarkdown: Teams renders Markdown (bold, italic, lists and links) in a TextBlock
// and in a FactSet's title and value, and a backslash escape is not honoured alike on every client (the mobile apps
// show the backslashes). A flow name is a tenant's text, and the deployment's channels receive every tenant's events:
// a flow named "[Reset your password](https://evil.example)" must not become a link. Every string of the card is a
// TextRun's text, which Adaptive Cards never parse as Markdown, kept as typed.
func TestTeamsWorkflow_TenantTextIsNeverMarkdown(t *testing.T) {
	const hostile = "[Reset your password](https://evil.example) **now** _x_\n- item"
	ev := sampleEvent()
	ev.FlowName = hostile
	ev.Title = "New findings in " + hostile
	ev.Message = "1. " + hostile
	msg := postTeamsWorkflow(t, ev)
	card := msg["attachments"].([]any)[0].(map[string]any)["content"].(map[string]any)

	var found int
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			typ, _ := v["type"].(string)
			if typ == "TextBlock" || typ == "FactSet" {
				t.Errorf("the card has a %s, which Teams renders as Markdown", typ)
			}
			for k, c := range v {
				if s, ok := c.(string); ok && strings.Contains(s, "evil.example") {
					found++
					if typ != "TextRun" || k != "text" {
						t.Errorf("tenant text in %s.%s, want only a TextRun's text", typ, k)
					}
				}
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(card["body"])
	if found < 3 {
		t.Errorf("the hostile text appears %d times, want it as typed in the title, the message and the flow fact", found)
	}
	texts := strings.Join(cardTexts(card), "\n")
	for _, want := range []string{ev.Title, ev.Message, hostile} {
		if !strings.Contains(texts, want) {
			t.Errorf("the card does not carry %q unchanged:\n%s", want, texts)
		}
	}
}

// cardTexts lists the texts of the card's TextRuns, a fact's title and value joined as one line.
func cardTexts(card map[string]any) []string {
	var out []string
	body, _ := card["body"].([]any)
	for _, el := range body {
		var line strings.Builder
		inlines, _ := el.(map[string]any)["inlines"].([]any)
		for _, in := range inlines {
			s, _ := in.(map[string]any)["text"].(string)
			line.WriteString(s)
		}
		out = append(out, line.String())
	}
	return out
}

// TestTeamsWorkflow_Validation: https only, unsigned (a secret is refused, as on teams: a Workflows trigger cannot
// verify it), and any public host, so a tenant in another region or cloud is not locked out.
func TestTeamsWorkflow_Validation(t *testing.T) {
	ctx := context.Background()
	spec, ok := Kind("teams-workflow")
	if !ok || spec.Scope != ScopeOrg|ScopeDeployment || spec.Signed || len(spec.Egress.Hosts) != 0 {
		t.Fatalf("teams-workflow = %+v, %v; want an unsigned org and deployment kind without a host list", spec, ok)
	}
	if _, has := spec.Field("secret"); has {
		t.Error("teams-workflow declares a secret")
	}
	for _, tc := range []struct {
		name   string
		values Values
		field  string
	}{
		{"plaintext", Values{"url": "http://prod-00.westeurope.logic.azure.com/workflows/x"}, "url"},
		{"secret", Values{"url": "https://203.0.113.10/workflows/x", "secret": "s"}, "secret"},
		{"no url", Values{}, "url"},
	} {
		err := Validate(ctx, "teams-workflow", ScopeOrg, tc.values, BlockInternalTargets)
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != tc.field {
			t.Errorf("%s: Validate = %v, want a refusal of %s", tc.name, err, tc.field)
		}
	}
	if err := Validate(ctx, "teams-workflow", ScopeDeployment, Values{"url": "https://203.0.113.10:443/workflows/x?sig=abc"}, BlockInternalTargets); err != nil {
		t.Errorf("a public https host: %v", err)
	}
}
