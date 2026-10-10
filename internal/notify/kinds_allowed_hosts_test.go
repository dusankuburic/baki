package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The deployment's host allow-list (PAD_NOTIFY_ALLOWED_HOSTS) restricts organisation channels: a URL whose host is
// off it does not validate, does not build, and is refused by the client before it dials.

// TestValidate_DeploymentAllowList: an off-list host is refused on url, before the name is resolved (a refused host
// costs no lookup, and the answer names no address); an on-list host is judged as before.
func TestValidate_DeploymentAllowList(t *testing.T) {
	ctx := context.Background()
	allowed := []string{"localhost", "*.hooks.example.com"}
	for _, raw := range []string{
		"https://unresolvable-host.invalid/x", // would answer "does not resolve" if it were looked up
		"https://hooks.example.com/x",         // the glob covers subdomains only
		"https://hooks.example.com.evil.example/x",
		"https://203.0.113.10/x",
		"http://127.0.0.1:9/x",
	} {
		err := Validate(ctx, "webhook", ScopeOrg, Values{"url": raw}, AllowLoopbackTargets, allowed...)
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != "url" {
			t.Errorf("%s: Validate = %v, want a refusal of url", raw, err)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "PAD_NOTIFY_ALLOWED_HOSTS") || strings.Contains(msg, "resolve") {
			t.Errorf("%s: %q, want the allow-list refusal, decided before any lookup", raw, msg)
		}
	}
	if err := Validate(ctx, "slack", ScopeOrg, Values{"url": "http://localhost:9/x"}, AllowLoopbackTargets, allowed...); err != nil {
		t.Errorf("an on-list host: %v", err)
	}
	// On the list is not enough: the egress policy still decides.
	if err := Validate(ctx, "slack", ScopeOrg, Values{"url": "http://localhost:9/x"}, BlockInternalTargets, allowed...); err == nil {
		t.Error("an on-list loopback host passed under BlockInternal")
	}
	// Unset is today's behaviour.
	if err := Validate(ctx, "webhook", ScopeOrg, Values{"url": "https://203.0.113.10/x"}, BlockInternalTargets); err != nil {
		t.Errorf("no allow-list: %v", err)
	}
}

// TestBuild_DeploymentAllowList: Build validates with the allow-list too, so the scanner's notifiers and the channel
// test refuse a stored channel whose host left the list.
func TestBuild_DeploymentAllowList(t *testing.T) {
	_, err := Build(context.Background(), "teams-workflow", ScopeOrg, Values{"url": "https://203.0.113.10/x"},
		Deps{Policy: BlockInternalTargets, AllowedHosts: []string{"*.logic.azure.com"}})
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Field != "url" {
		t.Errorf("Build = %v, want a refusal of url", err)
	}
}

// TestClients_DeploymentAllowListAtDelivery: the client of an allow-listed class refuses a request to a host off
// the list in its Proxy hook, which runs for every request, direct or proxied, before anything is dialled; a
// notifier built without validation cannot reach an off-list host either.
func TestClients_DeploymentAllowListAtDelivery(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]

	clients := NewClients(0)
	open := clients.For(AllowLoopbackTargets, EgressSpec{})
	restricted := clients.For(AllowLoopbackTargets, EgressSpec{}.WithAllowed([]string{"localhost"}))
	if restricted == open {
		t.Fatal("the allow-listed class shares the unrestricted client")
	}
	if again := clients.For(AllowLoopbackTargets, EgressSpec{}.WithAllowed([]string{"localhost"})); again != restricted {
		t.Error("the same allow-list got a second client")
	}

	off := &WebhookNotifier{URL: "http://127.0.0.1" + port + "/hook", Client: restricted, MaxAttempts: 1}
	err := off.Notify(context.Background(), sampleEvent())
	if !errors.Is(err, errHostNotOnAllowList) {
		t.Errorf("an off-list host: %v, want errHostNotOnAllowList", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the off-list host was reached %d times", n)
	}
	on := &WebhookNotifier{URL: "http://localhost" + port + "/hook", Client: restricted, MaxAttempts: 1}
	if err := on.Notify(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("an on-list host: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("hits = %d, want 1", n)
	}
}

// TestHostAllowed_ComparesWhatIsDialled: a host is compared in the ASCII form the transport dials (IDNA), so a
// spelling that maps onto a listed host is that host, and one that does not is refused.
func TestHostAllowed_ComparesWhatIsDialled(t *testing.T) {
	hosts := []string{"hooks.example.com", "*.contoso.com"}
	for host, want := range map[string]bool{
		"ｈｏｏｋｓ.example.com":      true, // fullwidth letters: dialled as hooks.example.com
		"HOOKS.example.com.":     true,
		"hooks.example.com.evil": false,
		"xhooks.example.com":     false,
		"a.contoso.com":          true,
		"a。contoso.com":          true, // an ideographic full stop separates labels: a.contoso.com
		"contoso.com":            false,
		"evil.com。contoso":       false,
		"bad host":               false,
		"":                       false,
	} {
		if got := hostAllowed(host, hosts); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}
