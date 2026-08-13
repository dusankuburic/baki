package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func sampleEvent() Event {
	return Event{
		Type:      EventDrift,
		FlowID:    "flow1",
		FlowName:  "Invoice Bot",
		Title:     "New findings in Invoice Bot",
		Message:   "2 new findings since baseline",
		NewErrors: 1, NewWarnings: 1,
	}
}

func TestWebhookNotifier_PostsEventJSON(t *testing.T) {
	var got Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("payload is not valid Event JSON: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &WebhookNotifier{URL: srv.URL, Client: srv.Client()}
	if err := n.Notify(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got.FlowID != "flow1" || got.Type != EventDrift || got.NewErrors != 1 {
		t.Errorf("webhook received unexpected event: %+v", got)
	}
}

func TestWebhookNotifier_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := &WebhookNotifier{URL: srv.URL, Client: srv.Client()}
	if err := n.Notify(context.Background(), sampleEvent()); err == nil {
		t.Error("expected an error for a 500 response")
	}
}

func TestTeamsNotifier_PostsMessageCard(t *testing.T) {
	var card map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &card); err != nil {
			t.Errorf("payload is not valid JSON: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &TeamsNotifier{URL: srv.URL, Client: srv.Client()}
	ev := sampleEvent()
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if card["@type"] != "MessageCard" {
		t.Errorf("@type = %v, want MessageCard", card["@type"])
	}
	if card["summary"] != ev.Title {
		t.Errorf("summary = %v, want %q", card["summary"], ev.Title)
	}
	if _, ok := card["sections"].([]any); !ok {
		t.Errorf("expected a sections array, got %T", card["sections"])
	}
}

func TestSlackNotifier_PostsAttachment(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("payload is not valid JSON: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &SlackNotifier{URL: srv.URL, Client: srv.Client()}
	ev := sampleEvent()
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if payload["text"] != ev.Title {
		t.Errorf("text = %v, want %q", payload["text"], ev.Title)
	}
	attachments, ok := payload["attachments"].([]any)
	if !ok || len(attachments) != 1 {
		t.Fatalf("expected one attachment, got %T", payload["attachments"])
	}
	att := attachments[0].(map[string]any)
	// Drift with errors → "danger" stripe.
	if att["color"] != "danger" {
		t.Errorf("color = %v, want danger for an event with errors", att["color"])
	}
	if att["footer"] != "Baki PAD Flow Analyzer" {
		t.Errorf("footer = %v", att["footer"])
	}
}

func TestSlackNotifier_AnalysisCompleteColorBySeverity(t *testing.T) {
	cases := []struct {
		name      string
		ev        Event
		wantColor string
	}{
		{"errors → danger", Event{Type: EventAnalysisComplete, Analysis: &AnalysisSummary{Errors: 1}}, "danger"},
		{"warnings → warning", Event{Type: EventAnalysisComplete, Analysis: &AnalysisSummary{Warnings: 2}}, "warning"},
		{"clean → good", Event{Type: EventAnalysisComplete, Analysis: &AnalysisSummary{}}, "good"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := slackColor(c.ev); got != c.wantColor {
				t.Errorf("slackColor = %q, want %q", got, c.wantColor)
			}
		})
	}
}

func TestSlackNotifier_IncludesAnalysisFields(t *testing.T) {
	ev := Event{
		Type:     EventAnalysisComplete,
		FlowName: "Onboarding",
		Analysis: &AnalysisSummary{Errors: 3, Warnings: 5, Info: 9, HealthScore: 42},
	}
	fields := slackFields(ev)
	// Expect Flow + Errors + Warnings + Info + Health = 5 short fields.
	if len(fields) != 5 {
		t.Fatalf("expected 5 fields for an analysis event, got %d (%+v)", len(fields), fields)
	}
	// First field is always Flow.
	if fields[0]["title"] != "Flow" || fields[0]["value"] != "Onboarding" {
		t.Errorf("first field = %+v, want Flow/Onboarding", fields[0])
	}
}

func TestSlackNotifier_HMACHeaderWhenSecretSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sig := r.Header.Get("X-Baki-Signature"); sig == "" {
			t.Error("expected X-Baki-Signature header when Secret is set")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &SlackNotifier{URL: srv.URL, Client: srv.Client(), Secret: "topsecret"}
	if err := n.Notify(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
}

func TestDispatcher_FansOutIncludingSlack(t *testing.T) {
	var webhookHits, slackHits atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slackHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer slack.Close()

	d, err := New(Config{WebhookURL: webhook.URL, SlackURL: slack.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.Dispatch(context.Background(), sampleEvent())

	if webhookHits.Load() != 1 {
		t.Errorf("webhook hit %d times, want 1", webhookHits.Load())
	}
	if slackHits.Load() != 1 {
		t.Errorf("slack hit %d times, want 1", slackHits.Load())
	}
}

func TestDispatcher_FansOutToAllChannels(t *testing.T) {
	var webhookHits, teamsHits atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	teams := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		teamsHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer teams.Close()

	d, err := New(Config{WebhookURL: webhook.URL, TeamsURL: teams.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !d.Enabled() {
		t.Fatal("expected dispatcher to be enabled with two channels")
	}
	d.Dispatch(context.Background(), sampleEvent())

	if webhookHits.Load() != 1 {
		t.Errorf("webhook hit %d times, want 1", webhookHits.Load())
	}
	if teamsHits.Load() != 1 {
		t.Errorf("teams hit %d times, want 1", teamsHits.Load())
	}
}

func TestDispatcher_DisabledWhenNoChannels(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.Enabled() {
		t.Error("dispatcher with no URLs should be disabled")
	}
	// No channels ⇒ Dispatch must be a harmless no-op.
	d.Dispatch(context.Background(), sampleEvent())
}

func TestDispatcher_NilSafe(t *testing.T) {
	var d *Dispatcher
	if d.Enabled() {
		t.Error("nil dispatcher must report disabled")
	}
	d.Dispatch(context.Background(), sampleEvent()) // must not panic
}

func TestDispatcher_FailingChannelDoesNotPropagateOrBlockOthers(t *testing.T) {
	var goodHits atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // always fails
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	// webhook (bad) first, teams (good) second — the good channel must still fire.
	d, err := New(Config{WebhookURL: bad.URL, TeamsURL: good.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.Dispatch(context.Background(), sampleEvent()) // logs the failure, does not panic

	if goodHits.Load() != 1 {
		t.Errorf("a failing channel blocked the healthy one: good hits = %d", goodHits.Load())
	}
}

func TestWebhookNotifier_RespectsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &WebhookNotifier{URL: srv.URL, Client: &http.Client{Timeout: 25 * time.Millisecond}}
	if err := n.Notify(context.Background(), sampleEvent()); err == nil {
		t.Error("expected a timeout error from a slow endpoint")
	}
}

// TestNew_RejectsPlaintextAlertURL is the regression test for the cleartext-
// alert leak: governance payloads carry internal flow names / finding counts,
// so a non-HTTPS alert URL must be rejected at construction rather than
// silently broadcasting those details in plaintext over the wire.
func TestNew_RejectsPlaintextAlertURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https accepted", "https://hooks.example.com/baki", false},
		{"http localhost accepted (dev)", "http://localhost:9000/hook", false},
		{"http 127.0.0.1 accepted (dev)", "http://127.0.0.1:9000/hook", false},
		{"plaintext remote rejected", "http://hooks.internal.local/baki", true},
		{"ftp rejected", "ftp://hooks.example.com/baki", true},
		{"empty accepted (channel disabled)", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(Config{WebhookURL: c.url})
			if c.wantErr && err == nil {
				t.Errorf("expected error for URL %q, got nil", c.url)
			}
			if !c.wantErr && err != nil {
				t.Errorf("unexpected error for URL %q: %v", c.url, err)
			}
		})
	}
}
