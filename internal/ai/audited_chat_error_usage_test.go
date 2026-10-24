package ai

import (
	"context"
	"errors"
	"testing"
)

// erroringChatProvider answers Chat with an error that carries the usage the
// provider was billed (a content-filter finish, an agent stopped for an MCP
// approval), the way the OpenAI-compatible and Responses paths attach it.
type erroringChatProvider struct {
	servedFakeProvider
	err error
}

func (f *erroringChatProvider) Chat(context.Context, Request) (*Response, error) {
	return nil, f.err
}

func TestAuditedProvider_ChatBillsUsageCarriedOnError(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)
	held := &heldUsageError{err: ErrContentFiltered, tokensIn: 120, tokensOut: 30, servedModel: "gpt-4o-2024-11-20"}
	inner := &erroringChatProvider{servedFakeProvider: servedFakeProvider{id: azureFoundryProviderID}, err: held}
	sink := newUsageSink()
	ap := NewAuditedProvider(inner, sink.recorder(), "u1", azureFoundryProviderID)

	_, err := ap.Chat(context.Background(), Request{Model: "dep", OrgID: "org-1"})
	if !errors.Is(err, ErrContentFiltered) {
		t.Fatalf("err = %v, want the provider's error unchanged", err)
	}
	m := sink.next(t)
	if m.PromptTokens != 120 || m.CompletionTokens != 30 || m.OrgID != "org-1" {
		t.Errorf("recorded %+v, want the usage carried on the error (120/30, org-1)", m)
	}
}

func TestAuditedProvider_ChatErrorWithoutUsageRecordsNothing(t *testing.T) {
	inner := &erroringChatProvider{servedFakeProvider: servedFakeProvider{id: azureFoundryProviderID}, err: ErrProviderDown}
	sink := newUsageSink()
	ap := NewAuditedProvider(inner, sink.recorder(), "u1", azureFoundryProviderID)
	if _, err := ap.Chat(context.Background(), Request{Model: "dep"}); !errors.Is(err, ErrProviderDown) {
		t.Fatalf("err = %v", err)
	}
	select {
	case m := <-sink.ch:
		t.Errorf("a failure that carried no usage must not be billed, got %+v", m)
	default:
	}
}
