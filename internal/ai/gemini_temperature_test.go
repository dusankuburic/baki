package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// captureGemini points the Gemini provider at a fake API that records each
// request body and answers with an empty candidate (Chat) or nothing (Stream).
func captureGemini(t *testing.T) (*GeminiProvider, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	prev := geminiBaseHost
	geminiBaseHost = srv.URL
	t.Cleanup(func() {
		srv.Close()
		geminiBaseHost = prev
	})
	return &GeminiProvider{apiKey: "key", client: srv.Client()}, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

// An explicit temperature of 0 (TemperatureSet) reaches Gemini — it used to
// be dropped by omitempty, so "deterministic" ran at the model's default —
// while an unset zero still leaves the model's default.
func TestGemini_ExplicitZeroTemperatureIsSent(t *testing.T) {
	p, bodies := captureGemini(t)
	msgs := []Message{{Role: "user", Content: "hi"}}
	cases := []struct {
		req      Request
		want     float64
		wantSent bool
	}{
		{Request{Model: "gemini-2.5-pro", Messages: msgs, Temperature: 0, TemperatureSet: true}, 0, true},
		{Request{Model: "gemini-2.5-pro", Messages: msgs}, 0, false},
		{Request{Model: "gemini-2.5-pro", Messages: msgs, Temperature: 0.4}, 0.4, true},
	}
	for i, c := range cases {
		if _, err := p.Chat(context.Background(), c.req); err != nil {
			t.Fatalf("case %d chat: %v", i, err)
		}
		_ = p.Stream(context.Background(), c.req, func(Chunk) {})
		got := bodies()
		for _, body := range got[len(got)-2:] {
			cfg, _ := body["generationConfig"].(map[string]any)
			temp, sent := cfg["temperature"]
			if sent != c.wantSent {
				t.Errorf("case %d: temperature sent = %v, want %v (config %v)", i, sent, c.wantSent, cfg)
				continue
			}
			if sent && temp != c.want {
				t.Errorf("case %d: temperature = %v, want %v", i, temp, c.want)
			}
		}
	}
}
