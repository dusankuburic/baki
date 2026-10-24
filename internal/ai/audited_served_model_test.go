package ai

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// D-42: the usage row used to carry only the id the caller asked for. For an
// Azure AI Foundry deployment alias, a model router or a Foundry agent that
// is a user-chosen name, so analytics could not show what was billed. The row
// now also records the model the provider reported answering — and only when
// it differs from the requested id, so plain providers keep an empty column.

// awaitAliasedStreamMetric drives one Stream through an alias-provider
// audited wrapper and returns the recorded row.
func awaitAliasedStreamMetric(t *testing.T, stub *auditStub, req Request) *interfaces.UsageMetric {
	t.Helper()
	ch := make(chan *interfaces.UsageMetric, 1)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error { ch <- m; return nil }
	ap := NewAuditedProvider(stub, rec, "user-1", azureFoundryProviderID)
	if err := ap.Stream(context.Background(), req, func(Chunk) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("usage metric was not recorded")
		return nil
	}
}

// An agent turn: the request names the agent, the response names the model
// behind it. Both land on the row.
func TestAudited_AgentTurnRecordsServedModel(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)
	stub := &auditStub{
		models: []ModelInfo{{ID: "agent:support-bot", DisplayName: "Agent · support-bot"}},
		resp:   &Response{TokensIn: 100, TokensOut: 20, ServedModel: "gpt-4o-2024-11-20"},
	}
	m := awaitAliasedMetric(t, stub, Request{Model: "agent:support-bot"})
	if m.Model != "agent:support-bot" {
		t.Errorf("Model = %q, want the requested agent id", m.Model)
	}
	if m.ServedModel != "gpt-4o-2024-11-20" {
		t.Errorf("ServedModel = %q, want %q (the model the agent answered with)", m.ServedModel, "gpt-4o-2024-11-20")
	}
}

// A custom-named deployment streamed: the served model arrives on a chunk
// and must reach the row the same way.
func TestAuditedStream_DeploymentRecordsServedModel(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)
	stub := &auditStub{
		models: []ModelInfo{{ID: "my-gpt4o"}},
		streamChunks: []Chunk{
			{Text: "hello", ServedModel: " gpt-4o "},
			{Done: true, TokensIn: 10, TokensOut: 5},
		},
	}
	m := awaitAliasedStreamMetric(t, stub, Request{Model: "my-gpt4o"})
	if m.Model != "my-gpt4o" {
		t.Errorf("Model = %q, want the deployment name", m.Model)
	}
	if m.ServedModel != "gpt-4o" {
		t.Errorf("ServedModel = %q, want %q (trimmed)", m.ServedModel, "gpt-4o")
	}
}

// A deployment named after its model, or a provider that reports nothing,
// records an empty ServedModel: the column only says something when it
// differs from Model, so the analytics grouping stays "by model" for them.
func TestAudited_ServedModelBlankWhenSameOrUnknown(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)

	same := &auditStub{
		models: []ModelInfo{{ID: "gpt-4o"}},
		resp:   &Response{TokensIn: 10, TokensOut: 5, ServedModel: "GPT-4o"},
	}
	if m := awaitAliasedMetric(t, same, Request{Model: "gpt-4o"}); m.ServedModel != "" {
		t.Errorf("ServedModel = %q, want empty when it equals the requested id (case-insensitively)", m.ServedModel)
	}

	unknown := &auditStub{
		models: []ModelInfo{{ID: "known"}},
		resp:   &Response{TokensIn: 10, TokensOut: 5},
	}
	if m := awaitMetric(t, unknown, Request{Model: "known"}); m.ServedModel != "" {
		t.Errorf("ServedModel = %q, want empty when the provider reported none", m.ServedModel)
	}
}

// awaitAliasedMetric runs one Chat through an audited wrapper whose provider label is an alias provider
// (azure-foundry). Recording is async; the metric arrives on the channel.
func awaitAliasedMetric(t *testing.T, stub *auditStub, req Request) *interfaces.UsageMetric {
	t.Helper()
	ch := make(chan *interfaces.UsageMetric, 1)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error { ch <- m; return nil }
	ap := NewAuditedProvider(stub, rec, "user-1", azureFoundryProviderID)
	if _, err := ap.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("usage metric was not recorded")
		return nil
	}
}
