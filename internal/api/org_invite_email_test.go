package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/collaboration"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// TestOrgInvite_EmailQueuedWithTheToken: creating an invite queues one org_invite email on the mail runner
// carrying the token the response returns, with the (user-controlled) org name escaped in the HTML part.
func TestOrgInvite_EmailQueuedWithTheToken(t *testing.T) {
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(context.Background(), `Acme <b>&</b> Co`, "u-admin")
	if err != nil {
		t.Fatal(err)
	}
	backend := testutil.NewFakeBackend()
	sink := &captureSend{}
	email := mailer.NewServiceWithMailer(sink, "https://app.example.com")
	h := NewOrgHandler(orgSvc, backend, nil, &SecurityConfig{LocalUserID: "u-admin"}, email,
		service.NewOrgRuleService(backend, backend, nil), nil, service.NewAuthzService(backend, orgSvc),
		service.NewChannelService(backend), service.NewUserDirectory(backend), testAuditRecorder(backend), nil)

	body, _ := json.Marshal(map[string]string{"email": "invitee@example.com", "role": "member"})
	rr := httptest.NewRecorder()
	mountedAs(h, "u-admin").ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/orgs/"+org.ID+"/invites", bytes.NewReader(body)))
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || resp.Token == "" {
		t.Fatalf("no token in %s (%v)", rr.Body.String(), err)
	}
	if err := email.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.sent) != 1 {
		t.Fatalf("%d emails, want 1", len(sink.sent))
	}
	msg := sink.sent[0]
	link := "https://app.example.com/#invite=" + resp.Token
	if msg.Template != "org_invite" || !strings.Contains(msg.Text, link) || !strings.Contains(msg.HTML, `href="`+link+`"`) {
		t.Errorf("invite email = %+v, want the link %s", msg, link)
	}
	if strings.Contains(msg.HTML, "<b>") || !strings.Contains(msg.HTML, "Acme &lt;b&gt;&amp;&lt;/b&gt; Co") {
		t.Errorf("the org name is not escaped in the HTML part:\n%s", msg.HTML)
	}
}
