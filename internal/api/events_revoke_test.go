package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"pad-analyzer/internal/auth"
)

// A revoked access token (a logout, a deleted API token) ends the SSE streams opened with it at once: they used to
// learn of it at a 2-minute blacklist check, and kept receiving the user's events until then. Streams of other
// tokens stay open.
func TestHandleEvents_RevocationEndsTheTokensStreams(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	m.heartbeatInterval = time.Hour
	m.SetRevocationChecker(func(string) bool { return false })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := &auth.Claims{UserID: "u1", RegisteredClaims: jwt.RegisteredClaims{ID: r.URL.Query().Get("jti")}}
		m.HandleEvents(w, r.WithContext(auth.WithClaims(r.Context(), claims)))
	}))
	defer srv.Close()

	open := func(jti string) <-chan struct{} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		t.Cleanup(cancel)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?jti="+jti, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
		return ended
	}
	revoked, other := open("session-1"), open("session-2")
	deadline := time.Now().Add(2 * time.Second)
	for m.revocationWatchers("session-1") == 0 || m.revocationWatchers("session-2") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the streams never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if n := m.Revoke("session-1"); n != 1 {
		t.Errorf("Revoke ended %d streams, want 1", n)
	}
	select {
	case <-revoked:
	case <-time.After(time.Second):
		t.Fatal("the revoked token's stream is still open")
	}
	select {
	case <-other:
		t.Fatal("another token's stream ended")
	case <-time.After(100 * time.Millisecond):
	}
	if n := m.Revoke("session-1"); n != 0 {
		t.Errorf("a second Revoke ended %d streams, want none", n)
	}
}

// End to end, as internal/di wires it: logging out ends that session's event stream at once; the same user's other
// session keeps its stream.
func TestHandleEvents_LogoutEndsThatSessionsStream(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	rt.eventManager.SubscribeRevocations(busOf(t, rt))
	srv := httptest.NewServer(rt)
	defer srv.Close()
	first, second := memberSession(t, rt, "u-owner"), memberSession(t, rt, "u-owner")

	stream := func(bearer string) <-chan struct{} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
		req.Header.Set("Authorization", bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/events: %d", resp.StatusCode)
		}
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
		return ended
	}
	firstStream, secondStream := stream(first), stream(second)

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/logout", first, map[string]string{}), http.StatusOK)
	select {
	case <-firstStream:
	case <-time.After(3 * time.Second):
		t.Fatal("the logged-out session's event stream is still open")
	}
	select {
	case <-secondStream:
		t.Fatal("the other session's event stream ended")
	case <-time.After(100 * time.Millisecond):
	}
}
