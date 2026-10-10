package notify

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/errreport"
)

// No test in this file reaches DNS or the network beyond loopback: every host is a literal IP, "localhost" or
// malformed, so validation short-circuits before a lookup.

// chans is a deployment channel list from kind/value pairs, the value being the kind's first field (url, or the
// email kind's to); an empty value is left out, as config.DeploymentChannels leaves out an unset variable.
func chans(kv ...string) []ChannelConfig {
	var out []ChannelConfig
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			continue
		}
		spec, _ := Kind(kv[i])
		out = append(out, ChannelConfig{Kind: kv[i], Values: Values{spec.Fields[0].Name: kv[i+1]}})
	}
	return out
}

func TestKinds_Registered(t *testing.T) {
	if got, want := KindIDs(ScopeOrg), []string{"email", "jira", "slack", "teams", "teams-workflow", "webhook"}; !slices.Equal(got, want) {
		t.Errorf("org kinds = %v, want %v", got, want)
	}
	if got, want := KindIDs(ScopeDeployment), []string{"email", "jira", "slack", "teams", "teams-workflow", "webhook"}; !slices.Equal(got, want) {
		t.Errorf("deployment kinds = %v, want %v", got, want)
	}
	for _, k := range Kinds(0) {
		for _, f := range k.FieldsIn(ScopeOrg) {
			// An organisation's email goes to members only: an address field would make the server a relay.
			if k.ID == "email" && !f.Members {
				t.Errorf("the organisation email kind declares %q, which is not a members field", f.Name)
			}
		}
		if k.Mail != (k.ID == "email") {
			t.Errorf("kind %s Mail = %v", k.ID, k.Mail)
		}
		if k.Signed != (k.ID == "webhook" || k.ID == "slack") {
			t.Errorf("kind %s Signed = %v", k.ID, k.Signed)
		}
	}
}

// TestRegister_RefusesBadSpecs: a proxy-trusting egress needs a host allow-list (a tenant-chosen host behind a
// trusted proxy is an SSRF hole), and kind IDs are unique.
func TestRegister_RefusesBadSpecs(t *testing.T) {
	build := func(Deps, Values) (AlertNotifier, error) { return nil, nil }
	cases := map[string]KindSpec{
		"trust proxy without hosts": {ID: "x-proxy", Scope: ScopeDeployment, Egress: EgressSpec{TrustProxy: true}, Build: build, Public: listedURLValues},
		"duplicate id":              {ID: "webhook", Scope: ScopeOrg, Build: build, Public: listedURLValues},
		"no scope":                  {ID: "x-scope", Build: build, Public: listedURLValues},
		"no builder":                {ID: "x-build", Scope: ScopeOrg, Public: listedURLValues},
		"upper-case id":             {ID: "Webhook2", Scope: ScopeOrg, Build: build, Public: listedURLValues},
		"duplicate field":           {ID: "x-field", Scope: ScopeOrg, Fields: []FieldSpec{{Name: "url"}, {Name: "url"}}, Build: build, Public: listedURLValues},
		"field of another scope":    {ID: "x-fscope", Scope: ScopeOrg, Fields: []FieldSpec{{Name: "to", Scope: ScopeDeployment}}, Build: build, Public: listedURLValues},
		"deployment members field":  {ID: "x-members", Scope: ScopeOrg | ScopeDeployment, Fields: []FieldSpec{{Name: "to", Members: true}}, Build: build, Public: listedURLValues},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("Register accepted %+v", spec)
				}
			}()
			Register(spec)
		})
	}
	if _, ok := Kind("x-proxy"); ok {
		t.Error("a refused kind was registered")
	}
}

// L-misc-7: notifiers of one egress class share one client, so connections pool instead of each channel save or
// test cloning a transport (and leaving its idle connections behind).
func TestClients_OnePerClass(t *testing.T) {
	c := NewClients(0)
	a := c.For(BlockInternalTargets, EgressSpec{})
	if a == nil || a.Transport == nil || a.CheckRedirect == nil {
		t.Fatalf("client = %+v, want a guarded client that does not follow redirects", a)
	}
	if b := c.For(BlockInternalTargets, EgressSpec{}); b != a {
		t.Error("the same class got a second client")
	}
	if b := c.For(AllowLoopbackTargets, EgressSpec{}); b == a {
		t.Error("another policy shared the client")
	}
	if b := c.For(BlockInternalTargets, EgressSpec{Hosts: []string{"*.atlassian.net"}, TrustProxy: true}); b == a {
		t.Error("a proxy-trusting class shared the strict client")
	}
	ctx := context.Background()
	deps := Deps{Client: a, Policy: BlockInternalTargets}
	w, err := Build(ctx, "webhook", ScopeOrg, Values{"url": "https://203.0.113.10/hook", "secret": "s"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Build(ctx, "slack", ScopeOrg, Values{"url": "https://203.0.113.10/slack"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if w.(*WebhookNotifier).Client != a || s.(*SlackNotifier).Client != a {
		t.Error("the notifiers do not share the caller's client")
	}
}

// L-misc-7: Validate refuses exactly what Build refuses, without building a client.
func TestValidate_RejectsWhatBuildRejects(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, kind string
		values     Values
		scope      Scope
		field      string
	}{
		{"unknown kind", "pager", Values{"url": "https://203.0.113.10/hook"}, ScopeOrg, "kind"},
		{"an address on an org email", "email", Values{"to": "ops@example.com", "recipients": "u1"}, ScopeOrg, "to"},
		{"members on the deployment's email", "email", Values{"to": "ops@example.com", "recipients": "u1"}, ScopeDeployment, "recipients"},
		{"org email without recipients", "email", Values{}, ScopeOrg, "recipients"},
		{"org email recipient twice", "email", Values{"recipients": "u1,u1"}, ScopeOrg, "recipients"},
		{"org jira project name", "jira", Values{"url": "https://203.0.113.10", "email": "a@b.c", "token": "t", "project": "My Project"}, ScopeOrg, "project"},
		{"org jira user with a colon", "jira", Values{"url": "https://203.0.113.10", "email": "a:b", "token": "t", "project": "PAD"}, ScopeOrg, "email"},
		{"org jira plaintext", "jira", Values{"url": "http://203.0.113.10", "email": "a@b.c", "token": "t", "project": "PAD"}, ScopeOrg, "url"},
		{"plaintext", "webhook", Values{"url": "http://example.com/hook"}, ScopeOrg, "url"},
		{"loopback literal", "slack", Values{"url": "https://127.0.0.1/hook"}, ScopeOrg, "url"},
		{"metadata", "teams", Values{"url": "https://169.254.169.254/latest"}, ScopeOrg, "url"},
		{"no host", "webhook", Values{"url": "https:///hook"}, ScopeOrg, "url"},
		{"garbage", "webhook", Values{"url": "://nope"}, ScopeOrg, "url"},
		{"missing url", "webhook", Values{"secret": "s"}, ScopeOrg, "url"},
		{"secret on teams", "teams", Values{"url": "https://203.0.113.10/hook", "secret": "s"}, ScopeOrg, "secret"},
		{"secret on teams-workflow", "teams-workflow", Values{"url": "https://203.0.113.10/hook", "secret": "s"}, ScopeOrg, "secret"},
		{"jira without token", "jira", Values{"url": "https://203.0.113.10", "email": "a@b.c", "project": "PAD"}, ScopeDeployment, "token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vErr := Validate(ctx, tc.kind, tc.scope, tc.values, BlockInternalTargets)
			_, bErr := Build(ctx, tc.kind, tc.scope, tc.values, Deps{Policy: BlockInternalTargets})
			var fe *FieldError
			if !errors.As(vErr, &fe) || fe.Field != tc.field || bErr == nil {
				t.Errorf("Validate=%v Build=%v, want both to refuse field %s", vErr, bErr, tc.field)
			}
		})
	}
	for _, tc := range []struct {
		kind   string
		values Values
		policy TargetPolicy
	}{
		{"webhook", Values{"url": "https://203.0.113.10/hook"}, BlockInternalTargets},
		{"slack", Values{"url": "http://localhost:9999/dev"}, AllowLoopbackTargets},
	} {
		if err := Validate(ctx, tc.kind, ScopeOrg, tc.values, tc.policy); err != nil {
			t.Errorf("Validate(%s, %v) = %v, want ok", tc.kind, tc.values, err)
		}
	}
}

// TestValidate_ErrorsNeverQuoteTheURL: for Slack and Teams the URL is the credential, and a validation error is
// logged by the scanner and answered to the admin (B4).
func TestValidate_ErrorsNeverQuoteTheURL(t *testing.T) {
	for _, raw := range []string{
		"http://hooks.slack.com/services/T000/B000/XXXXSECRET",
		"https://127.0.0.1/services/T000/B000/XXXXSECRET?token=abc",
		"ftp://hooks.example.com/XXXXSECRET",
		"https://203.0.113.10/x\x7fXXXXSECRET",
	} {
		err := Validate(context.Background(), "slack", ScopeOrg, Values{"url": raw}, BlockInternalTargets)
		if err == nil {
			t.Fatalf("%q validated", raw)
		}
		if strings.Contains(err.Error(), "XXXXSECRET") || strings.Contains(err.Error(), "token=") {
			t.Errorf("validation error quotes the URL: %v", err)
		}
	}
}

func TestEgressSpec_HostAllowList(t *testing.T) {
	hosts := []string{"*.atlassian.net", "jira.example.com"}
	for host, want := range map[string]bool{
		"acme.atlassian.net": true, "a.b.atlassian.net": true, "atlassian.net": false, "evilatlassian.net": false,
		"jira.example.com": true, "JIRA.example.com.": true, "x.jira.example.com": false,
	} {
		if got := hostAllowed(host, hosts); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
	spec := EgressSpec{Hosts: hosts}
	if err := checkAlertURL(context.Background(), "https://203.0.113.10/x", BlockInternalTargets, spec); err == nil {
		t.Error("a host off the allow-list validated")
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://hooks.slack.com/services/T/B/SECRET":          "https://hooks.slack.com/…",
		"https://user:pass@example.com:8443/hook?sig=abc#frag": "https://example.com:8443/…",
		"https://acme.webhook.office.com/webhookb2/abc/def":    "https://acme.webhook.office.com/…",
		"not a url":         "…",
		"":                  "…",
		"://missing-scheme": "…",
	}
	for in, want := range cases {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestListedURL: the API's form keeps the host only, has no scheme (so it never validates as a channel URL) and
// carries the marker HasRedactionMarker finds.
func TestListedURL(t *testing.T) {
	cases := map[string]string{
		"https://hooks.slack.com/services/T/B/SECRET":          "hooks.slack.com/…",
		"https://user:pass@example.com:8443/hook?sig=abc#frag": "example.com:8443/…",
		"http://127.0.0.1:9/x":                                 "127.0.0.1:9/…",
		"not a url":                                            "…",
		"":                                                     "…",
	}
	for in, want := range cases {
		got := ListedURL(in)
		if got != want {
			t.Errorf("ListedURL(%q) = %q, want %q", in, got, want)
		}
		if !HasRedactionMarker(got) {
			t.Errorf("HasRedactionMarker(%q) = false", got)
		}
		if _, err := validateAlertURL(got, AllowLoopbackTargets); err == nil {
			t.Errorf("the listing %q validates as a channel URL", got)
		}
	}
	for in, want := range map[string]bool{
		"https://x.example/%E2%80%A6": true, "https://x.example/%e2%80%a6?a=1": true, "https://x.example/a...b": false,
		"https://x.example/hook": false,
	} {
		if got := HasRedactionMarker(in); got != want {
			t.Errorf("HasRedactionMarker(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"type":"drift"}`)
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := signV2("s3cret", now.Unix(), body)
	if err := VerifySignature("s3cret", ts, sig, body, now.Add(time.Minute)); err != nil {
		t.Fatalf("a fresh signature: %v", err)
	}
	for name, err := range map[string]error{
		"other secret":    VerifySignature("other", ts, sig, body, now),
		"other body":      VerifySignature("s3cret", ts, sig, []byte(`{"type":"test"}`), now),
		"stale":           VerifySignature("s3cret", ts, sig, body, now.Add(SignatureTolerance+time.Second)),
		"future":          VerifySignature("s3cret", ts, sig, body, now.Add(-SignatureTolerance-time.Second)),
		"other timestamp": VerifySignature("s3cret", strconv.FormatInt(now.Unix()+1, 10), sig, body, now),
		"legacy header":   VerifySignature("s3cret", ts, signBody("s3cret", body), body, now),
		"bad timestamp":   VerifySignature("s3cret", "soon", sig, body, now),
		"no secret":       VerifySignature("", ts, sig, body, now),
	} {
		if err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

// TestSignedDelivery_HeadersPerAttempt: the delivery id stays across retries, the timestamp and the V2 signature are
// the attempt's, and the legacy signature covers the body.
func TestSignedDelivery_HeadersPerAttempt(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		bodies = append(bodies, buf.Bytes())
		first := len(seen) == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	clock := time.Unix(1_800_000_000, 0)
	env := Envelope{Now: func() time.Time { clock = clock.Add(time.Second); return clock }, UserAgent: UserAgent("1.2.3")}
	n := &WebhookNotifier{URL: srv.URL, Client: srv.Client(), Secret: "s3cret", Envelope: env, Backoff: time.Millisecond}
	if err := n.Notify(context.Background(), sampleEvent()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("%d attempts, want 2", len(seen))
	}
	if seen[0].Get(HeaderDelivery) == "" || seen[0].Get(HeaderDelivery) != seen[1].Get(HeaderDelivery) {
		t.Errorf("delivery ids %q, %q: want one id across retries", seen[0].Get(HeaderDelivery), seen[1].Get(HeaderDelivery))
	}
	if seen[0].Get(HeaderTimestamp) == seen[1].Get(HeaderTimestamp) {
		t.Error("the retry reused the first attempt's timestamp")
	}
	for i, h := range seen {
		if err := VerifySignature("s3cret", h.Get(HeaderTimestamp), h.Get(HeaderSignatureV2), bodies[i], clock); err != nil {
			t.Errorf("attempt %d: %v", i+1, err)
		}
		if h.Get(HeaderSignature) != signBody("s3cret", bodies[i]) {
			t.Errorf("attempt %d: legacy signature %q", i+1, h.Get(HeaderSignature))
		}
		if h.Get(HeaderEvent) != "drift" || h.Get("User-Agent") != "baki/1.2.3" {
			t.Errorf("attempt %d: event %q, user agent %q", i+1, h.Get(HeaderEvent), h.Get("User-Agent"))
		}
	}
	unsigned := &TeamsNotifier{URL: srv.URL, Client: srv.Client()}
	if err := unsigned.Notify(context.Background(), sampleEvent()); err != nil {
		t.Fatal(err)
	}
	if h := seen[len(seen)-1]; h.Get(HeaderSignature) != "" || h.Get(HeaderSignatureV2) != "" || h.Get(HeaderDelivery) == "" {
		t.Errorf("teams headers %v: want the envelope without signatures", h)
	}
}

// recordingSink is an errreport sink that keeps the reported errors' text.
type recordingSink struct {
	mu   sync.Mutex
	errs []string
}

func (s *recordingSink) CaptureError(_ context.Context, err error, _ errreport.Attrs) {
	s.mu.Lock()
	s.errs = append(s.errs, err.Error())
	s.mu.Unlock()
}

func (s *recordingSink) CapturePanic(context.Context, any, []byte, errreport.Attrs) {}

// TestDeliver_FailingSlackLeaksNoToken is the regression gate for B4: the client's transport error names the whole
// URL, which for Slack and Teams is the credential, and it went into the WARN log and the error sink.
func TestDeliver_FailingSlackLeaksNoToken(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	sink := &recordingSink{}
	errreport.Register(sink)
	defer errreport.Register(nil)

	// Port 1 on loopback refuses the connection; the path and query carry the "credential".
	const secretURL = "http://127.0.0.1:1/services/T000/B000/XXXXSECRET?token=abc"
	n, err := Build(context.Background(), "slack", ScopeOrg, Values{"url": secretURL}, Deps{Policy: AllowLoopbackTargets, MaxAttempts: 2, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	Deliver(context.Background(), n, sampleEvent())

	sink.mu.Lock()
	reported := strings.Join(sink.errs, "\n")
	sink.mu.Unlock()
	if reported == "" {
		t.Fatal("the failed delivery was not reported")
	}
	for name, text := range map[string]string{"log": logs.String(), "error report": reported} {
		if strings.Contains(text, "XXXXSECRET") || strings.Contains(text, "token=abc") {
			t.Errorf("the %s carries the channel URL's path or query:\n%s", name, text)
		}
		if !strings.Contains(text, "127.0.0.1:1/…") {
			t.Errorf("the %s does not name the redacted target:\n%s", name, text)
		}
	}
}

// TestValidate_NamesNoResolvedAddress: a refused target's error names the host, never the address it resolved to,
// so a channel save cannot map the server's internal DNS (B13).
func TestValidate_NamesNoResolvedAddress(t *testing.T) {
	err := Validate(context.Background(), "webhook", ScopeOrg, Values{"url": "https://10.1.2.3/hook"}, BlockInternalTargets)
	if err == nil || !strings.Contains(err.Error(), "internal targets are not allowed") {
		t.Fatalf("Validate = %v, want the internal-target refusal", err)
	}
	if err := Validate(context.Background(), "webhook", ScopeOrg, Values{"url": "https://localhost/hook"}, BlockInternalTargets); err == nil ||
		strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "::1") {
		t.Errorf("Validate(localhost) = %v, want a refusal without the resolved address", err)
	}
}
