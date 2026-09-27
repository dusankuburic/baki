package service

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// hostedFakeProvider streams what a Foundry agent turn looks like: a hosted
// tool step (start + done) the provider ran itself, then text, then Done.
type hostedFakeProvider struct {
	*testutil.FakeProvider
	requests []ai.Request
}

func (p *hostedFakeProvider) SupportsTools() bool { return true }

func (p *hostedFakeProvider) Stream(_ context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	p.requests = append(p.requests, req)
	onChunk(ai.Chunk{ToolEvent: &ai.ToolEvent{Name: "web_search_call", Label: "Agent · web search"}})
	onChunk(ai.Chunk{}) // keepalive during a long hosted run
	onChunk(ai.Chunk{ToolEvent: &ai.ToolEvent{Name: "web_search_call", Label: "Agent · web search", Done: true, OK: true, DurationMs: 12, Summary: "3 results"}})
	onChunk(ai.Chunk{Text: "answer"})
	onChunk(ai.Chunk{Done: true, TokensIn: 3, TokensOut: 2})
	return nil
}

func runHostedStream(t *testing.T, model string, useTools bool) (*testutil.CountingNotifier, *hostedFakeProvider) {
	t.Helper()
	fake := &hostedFakeProvider{FakeProvider: &testutil.FakeProvider{}}
	ai.RegisterProvider("hosted-fake", func(string) ai.Provider { return fake })
	notifier := &testutil.CountingNotifier{}
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	svc := &ChatService{
		notifier:      notifier,
		configDir:     t.TempDir(),
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
		settings:      nilSettings{},
		chatCtxCache:  newChatContextCache(),
	}
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}
	id, err := svc.StreamChatMessage(context.Background(), "test", doc, nil, models.ChatRequest{
		Provider: "hosted-fake", Model: model, UserMessage: "what is new?", UseTools: useTools,
	})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	svc.BeginStream(context.Background(), id)
	deadline := time.After(5 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("stream did not finish within 5s")
		case <-time.After(20 * time.Millisecond):
		}
	}
	return notifier, fake
}

// eventTypes lists the chat:event types in emission order.
func eventTypes(n *testutil.CountingNotifier) (types []string, payloads []map[string]interface{}) {
	for _, ev := range n.Events() {
		if ev.Name != "chat:event" {
			continue
		}
		env, ok := ev.Data.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := env["type"].(string)
		data, _ := env["data"].(map[string]interface{})
		types = append(types, typ)
		payloads = append(payloads, data)
	}
	return types, payloads
}

func indexOf(types []string, want string) int {
	for i, t := range types {
		if t == want {
			return i
		}
	}
	return -1
}

func TestStream_HostedToolEventsReachTheTrail(t *testing.T) {
	for _, tc := range []struct {
		name     string
		model    string
		useTools bool
	}{
		{"plain stream path", "gpt-4o", false},
		{"tool loop path", "gpt-4o", true},
		{"agent forces the tool loop", "agent:Helper", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notifier, fake := runHostedStream(t, tc.model, tc.useTools)
			types, payloads := eventTypes(notifier)

			start, done, chunk, end := indexOf(types, "tool"), indexOf(types, "tool_result"), indexOf(types, "chunk"), indexOf(types, "done")
			if start < 0 || done < 0 || chunk < 0 || end < 0 {
				t.Fatalf("events = %v", types)
			}
			if start >= done || done >= chunk || chunk >= end {
				t.Errorf("hosted tool events out of order: %v", types)
			}
			if payloads[start]["name"] != "web_search_call" || payloads[start]["label"] != "Agent · web search" {
				t.Errorf("tool event = %v", payloads[start])
			}
			if payloads[done]["ok"] != true || payloads[done]["summary"] != "3 results" || payloads[done]["durationMs"] != int64(12) {
				t.Errorf("tool_result event = %v", payloads[done])
			}
			if len(fake.requests) != 1 {
				t.Fatalf("provider called %d times", len(fake.requests))
			}
			// The tool loop offers baki's tools to the provider (the Azure
			// provider itself decides not to send them for an agent).
			if tc.useTools || ai.IsAgentModel(tc.model) {
				if len(fake.requests[0].Tools) == 0 {
					t.Error("tool loop did not run: no tool definitions on the request")
				}
			} else if len(fake.requests[0].Tools) != 0 {
				t.Error("plain path must not offer tools")
			}
		})
	}
}
