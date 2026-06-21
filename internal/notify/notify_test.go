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
	if err := n.Notify(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if card["@type"] != "MessageCard" {
		t.Errorf("@type = %v, want MessageCard", card["@type"])
	}
	if card["summary"] != "New findings in Invoice Bot" {
		t.Errorf("summary = %v", card["summary"])
	}
	if _, ok := card["sections"].([]any); !ok {
		t.Errorf("expected a sections array, got %T", card["sections"])
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

	d := New(Config{WebhookURL: webhook.URL, TeamsURL: teams.URL})
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
	d := New(Config{})
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
	d := New(Config{WebhookURL: bad.URL, TeamsURL: good.URL})
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
