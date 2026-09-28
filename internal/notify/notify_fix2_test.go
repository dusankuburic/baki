package notify

import (
	"context"
	"testing"
)

// No test in this file reaches DNS or the network: every host is a literal
// IP, "localhost" or malformed, so validation short-circuits before a lookup.

// L-misc-7: notifiers built on a shared client reuse its transport and
// connection pool instead of cloning a transport (and leaving an idle TLS
// connection and its goroutines behind) per channel save or test.
func TestBuildChannelNotifierWith_SharesClient(t *testing.T) {
	client := NewChannelClient(BlockInternalTargets)
	if client == nil || client.Transport == nil || client.CheckRedirect == nil {
		t.Fatalf("NewChannelClient = %+v, want a guarded client that does not follow redirects", client)
	}
	ctx := context.Background()
	w, err := BuildChannelNotifierWith(ctx, client, "webhook", "https://203.0.113.10/hook", "s", BlockInternalTargets)
	if err != nil {
		t.Fatal(err)
	}
	s, err := BuildChannelNotifierWith(ctx, client, "slack", "https://203.0.113.10/slack", "s", BlockInternalTargets)
	if err != nil {
		t.Fatal(err)
	}
	tm, err := BuildChannelNotifierWith(ctx, client, "teams", "https://203.0.113.10/teams", "", BlockInternalTargets)
	if err != nil {
		t.Fatal(err)
	}
	if w.(*WebhookNotifier).Client != client || s.(*SlackNotifier).Client != client || tm.(*TeamsNotifier).Client != client {
		t.Error("the notifiers do not share the caller's client")
	}
	if _, err := BuildChannelNotifierWith(ctx, client, "pager", "https://203.0.113.10/x", "", BlockInternalTargets); err == nil {
		t.Error("an unknown kind must be refused")
	}
	// A nil client gets one of its own; the old entry point still works, on
	// a client of its own.
	if n, err := BuildChannelNotifierWith(ctx, nil, "webhook", "https://203.0.113.10/hook", "s", BlockInternalTargets); err != nil || n.(*WebhookNotifier).Client == nil || n.(*WebhookNotifier).Client == client {
		t.Errorf("nil client: %v, %v", n, err)
	}
	if n, err := BuildChannelNotifier("webhook", "https://203.0.113.10/hook", "s", BlockInternalTargets); err != nil || n.(*WebhookNotifier).Client == client {
		t.Errorf("BuildChannelNotifier = %v, %v", n, err)
	}
}

// L-misc-7: ValidateChannel refuses exactly what BuildChannelNotifier
// refuses, without building a client.
func TestValidateChannel_RejectsWhatBuildRejects(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, kind, url string
		policy          TargetPolicy
	}{
		{"unknown kind", "email", "https://203.0.113.10/hook", BlockInternalTargets},
		{"plaintext", "webhook", "http://example.com/hook", BlockInternalTargets},
		{"loopback literal", "slack", "https://127.0.0.1/hook", BlockInternalTargets},
		{"metadata", "teams", "https://169.254.169.254/latest", BlockInternalTargets},
		{"no host", "webhook", "https:///hook", BlockInternalTargets},
		{"garbage", "webhook", "://nope", BlockInternalTargets},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vErr := ValidateChannel(ctx, tc.kind, tc.url, tc.policy)
			_, bErr := BuildChannelNotifier(tc.kind, tc.url, "", tc.policy)
			if vErr == nil || bErr == nil {
				t.Errorf("ValidateChannel=%v BuildChannelNotifier=%v, want both to refuse", vErr, bErr)
			}
		})
	}
	for _, tc := range []struct {
		kind, url string
		policy    TargetPolicy
	}{
		{"webhook", "https://203.0.113.10/hook", BlockInternalTargets},
		{"slack", "http://localhost:9999/dev", AllowLoopbackTargets},
	} {
		if err := ValidateChannel(ctx, tc.kind, tc.url, tc.policy); err != nil {
			t.Errorf("ValidateChannel(%s, %s) = %v, want ok", tc.kind, tc.url, err)
		}
	}
}
