package mail

import (
	"strings"
	"testing"
)

// TestTemplates_EscapeUserText: user-controlled fields are escaped in HTML and kept verbatim in text.
func TestTemplates_EscapeUserText(t *testing.T) {
	evil := `<img src=x onerror=alert(1)>"Corp"`
	msg, err := OrgInvite.Render(OrgInviteData{OrgName: evil, Token: "TOK"}, Env{BaseURL: goldenBase}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.HTML, "<img") || !strings.Contains(msg.HTML, "&lt;img") {
		t.Errorf("org name must be HTML-escaped in the invite body:\n%s", msg.HTML)
	}
	if !strings.Contains(msg.Text, evil) {
		t.Errorf("the text part carries the name verbatim:\n%s", msg.Text)
	}
}

// TestTemplates_LinksAreEscapedInAttributes: the token links used to be pasted into href unescaped; html/template
// now escapes them for the attribute and neutralises a non-http scheme. The base URL is operator config, the token
// ours, so this is defence in depth.
func TestTemplates_LinksAreEscapedInAttributes(t *testing.T) {
	msg, err := PasswordReset.Render(PasswordResetData{Token: `x" onmouseover="alert(1)`}, Env{BaseURL: goldenBase}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.HTML, `" onmouseover="`) {
		t.Errorf("a quote in the link broke out of the href:\n%s", msg.HTML)
	}
	msg, err = EmailVerification.Render(EmailVerificationData{Token: "T"}, Env{BaseURL: "javascript:alert(1)//"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.HTML, `href="javascript:`) {
		t.Errorf("a javascript: link reached the href:\n%s", msg.HTML)
	}
}

// TestTemplates_TokenLinks: each token email links to its frontend route, or carries the bare token without a base
// URL (still recoverable from the log-only mailer).
func TestTemplates_TokenLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		mail func(Env) (Message, error)
		want string
	}{
		{"reset", func(e Env) (Message, error) { return PasswordReset.Render(PasswordResetData{Token: "T1"}, e, "") }, "/#resetPassword=T1"},
		{"verify", func(e Env) (Message, error) {
			return EmailVerification.Render(EmailVerificationData{Token: "T2"}, e, "")
		}, "/#verifyEmail=T2"},
		{"invite", func(e Env) (Message, error) {
			return OrgInvite.Render(OrgInviteData{OrgName: "Acme", Token: "T3"}, e, "")
		}, "/#invite=T3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := tc.mail(Env{BaseURL: "https://app.example.com"})
			if err != nil {
				t.Fatal(err)
			}
			want := "https://app.example.com" + tc.want
			if !strings.Contains(msg.Text, want) || !strings.Contains(msg.HTML, `href="`+want+`"`) {
				t.Errorf("link %q missing:\ntext=%s\nhtml=%s", want, msg.Text, msg.HTML)
			}
			if !msg.Sensitive {
				t.Error("a token email must be Sensitive")
			}
			bare, err := tc.mail(Env{})
			if err != nil {
				t.Fatal(err)
			}
			token := tc.want[strings.Index(tc.want, "=")+1:]
			if !strings.Contains(bare.Text, "\n"+token) || strings.Contains(bare.Text, "#") {
				t.Errorf("without a base URL the text carries the bare token:\n%s", bare.Text)
			}
		})
	}
}

// TestFindingEmails_LinkOnlyWithBaseURL: the finding emails link to the app (where the targeted alert waits) when a
// base URL is configured, and carry no link otherwise (a relative href resolves against the mail client).
func TestFindingEmails_LinkOnlyWithBaseURL(t *testing.T) {
	assigned := FindingAssignedData{AssigneeName: "Una", AssignerName: "Ari", FlowName: "Invoices", FindingTitle: "Unhandled error"}
	comment := FindingCommentData{RecipientName: "Una", CommenterName: "Ari", FlowName: "Invoices", FindingTitle: "Unhandled error", Comment: "looks risky"}
	for _, env := range []Env{{BaseURL: "https://app.example.com"}, {}} {
		a, err := FindingAssigned.Render(assigned, env, "")
		if err != nil {
			t.Fatal(err)
		}
		c, err := FindingComment.Render(comment, env, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range []Message{a, c} {
			linked := strings.Contains(msg.HTML, `<a href="https://app.example.com/">View the finding</a>`) && strings.Contains(msg.Text, "https://app.example.com/")
			if linked != (env.BaseURL != "") || (env.BaseURL == "" && (strings.Contains(msg.HTML, "<a ") || strings.Contains(msg.Text, "View it"))) {
				t.Errorf("%s with base %q: linked=%v\ntext=%s\nhtml=%s", msg.Template, env.BaseURL, linked, msg.Text, msg.HTML)
			}
			if msg.Sensitive {
				t.Errorf("%s carries no token and must not be Sensitive", msg.Template)
			}
		}
	}
}

// TestFindingEmails_WithoutFlowNameDoNotNameTheFlow: a recipient who cannot read the flow gets FlowName "" and the
// email leaves the flow clause out (it used to print `on the flow "":`).
func TestFindingEmails_WithoutFlowNameDoNotNameTheFlow(t *testing.T) {
	a, err := FindingAssigned.Render(FindingAssignedData{AssigneeName: "Una", AssignerName: "Ari", FindingTitle: "Unused variable"}, Env{}, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := FindingComment.Render(FindingCommentData{RecipientName: "Una", CommenterName: "Ari", FindingTitle: "Unused variable", Comment: "hi"}, Env{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []Message{a, c} {
		if strings.Contains(msg.Text, "on the flow") || strings.Contains(msg.HTML, "on the flow") {
			t.Errorf("%s names a flow it was not given:\n%s\n%s", msg.Template, msg.Text, msg.HTML)
		}
		if !strings.Contains(msg.Text, "assigned to you:") && !strings.Contains(msg.Text, "a finding to you:") {
			t.Errorf("%s lost its sentence:\n%s", msg.Template, msg.Text)
		}
	}
}

// TestFindingComment_QuotesAPreview: a long comment is cut to its preview in both parts.
func TestFindingComment_QuotesAPreview(t *testing.T) {
	long := strings.Repeat("x", commentPreviewBytes) + "TAIL"
	msg, err := FindingComment.Render(FindingCommentData{RecipientName: "Una", CommenterName: "Ari", FindingTitle: "t", Comment: long}, Env{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.Text, "TAIL") || strings.Contains(msg.HTML, "TAIL") || !strings.Contains(msg.Text, "…") {
		t.Errorf("the comment was not cut to its preview:\n%s", msg.Text)
	}
}

// TestRender_LocaleDefaultsToEnglish: English is the only locale; any requested locale renders it and says so.
func TestRender_LocaleDefaultsToEnglish(t *testing.T) {
	for _, loc := range []Locale{"", "en", "de"} {
		msg, err := PasswordReset.Render(PasswordResetData{Token: "T"}, Env{}, loc)
		if err != nil {
			t.Fatal(err)
		}
		if msg.Locale != DefaultLocale || !strings.HasPrefix(msg.Subject, "Reset your") {
			t.Errorf("locale %q rendered %q in %q", loc, msg.Subject, msg.Locale)
		}
	}
}

// TestRegister_RefusesBadTemplates: a duplicate ID, a parse error or a field the data type lacks fails at
// registration (package init), not at the first send.
func TestRegister_RefusesBadTemplates(t *testing.T) {
	type data struct{ Name string }
	for name, tmpl := range map[string]Template[data]{
		"duplicate id":  {ID: PasswordReset.ID(), Subject: "s", Text: "t"},
		"parse error":   {ID: "test_parse", Subject: "s", Text: "{{.Data.Name"},
		"unknown field": {ID: "test_field", Subject: "s", Text: "{{.Data.Missing}}"},
		"html field":    {ID: "test_html", Subject: "s", Text: "t", HTML: "<p>{{.Data.Missing}}</p>"},
		"no subject":    {ID: "test_subject", Text: "t"},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Register did not panic")
				}
			}()
			Register(tmpl)
		})
	}
	for _, id := range IDs() {
		if strings.HasPrefix(id, "test_") {
			t.Errorf("a refused template was registered: %s", id)
		}
	}
}

// TestRenderGovernanceAlert_MirrorsTheEvent: the exported renderer the notify email kind calls.
func TestRenderGovernanceAlert_MirrorsTheEvent(t *testing.T) {
	msg, err := RenderGovernanceAlert(GovernanceAlertData{Event: "health_regression", FlowID: "f1", Title: "Health dropped", HealthScore: 50, PrevHealth: 90, At: goldenAt})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Template != "governance_alert" || msg.Subject != "[HEALTH_REGRESSION] Health dropped" || msg.Sensitive {
		t.Errorf("message = %+v", msg)
	}
	for _, want := range []string{"Flow: f1\n", "Health: 90 → 50\n", "Time: 2026-10-10T12:00:00Z\n"} {
		if !strings.Contains(msg.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, msg.Text)
		}
	}
}
