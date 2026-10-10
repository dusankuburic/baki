package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// captureLogs routes the default logger into a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestChat_UnknownProviderIsClientError (survey bug 3): a turn naming a provider the registry does not know is
// the client's mistake, refused before any stream starts with ErrProviderUnknown — never the "provider setup
// failed" WARN that reads as an outage in the logs, and never the generic "try again" setup error. The context
// preview, which resolves exactly as the turn does, answers the same.
func TestChat_UnknownProviderIsClientError(t *testing.T) {
	registerFakeProvider()
	logs := captureLogs(t)
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	svc := &ChatService{
		notifier:      &testutil.CountingNotifier{},
		configDir:     t.TempDir(),
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
		settings:      nilSettings{},
		chatCtxCache:  newChatContextCache(),
	}
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}
	req := models.ChatRequest{Provider: "nope", Model: "m", UserMessage: "hi"}

	_, err := svc.StreamChatMessage(context.Background(), "u1", doc, nil, req)
	if !errors.Is(err, ErrProviderUnknown) {
		t.Fatalf("StreamChatMessage(provider nope): err = %v, want ErrProviderUnknown", err)
	}
	if errors.Is(err, errProviderSetup) {
		t.Error("an unknown provider id is not a setup failure")
	}
	if _, err := svc.PreviewContext(context.Background(), "u1", doc, nil, req); !errors.Is(err, ErrProviderUnknown) {
		t.Errorf("PreviewContext(provider nope): err = %v, want ErrProviderUnknown", err)
	}
	if got := logs.String(); strings.Contains(got, "provider setup failed") {
		t.Errorf("a client typo was logged as an operator incident:\n%s", got)
	}
	// A stream that nevertheless meets the factory's refusal (a provider unregistered mid-flight) is classified
	// for the client, not replaced by the setup error.
	if got := providerSetupError("u1", "nope", ai.ErrUnknownProvider); !errors.Is(got, ai.ErrUnknownProvider) {
		t.Errorf("providerSetupError(ErrUnknownProvider) = %v, want the sentinel kept", got)
	}
	ctl := &streamCtl{}
	if code, msg := ctl.classifyFailure(context.Background(), ai.ErrUnknownProvider); code != ChatErrProviderUnknown || !strings.Contains(msg, "unknown") {
		t.Errorf("classifyFailure(ErrUnknownProvider) = %q, %q", code, msg)
	}
	if got := logs.String(); strings.Contains(got, "provider setup failed") {
		t.Errorf("the factory's refusal of an unknown id was logged as a setup failure:\n%s", got)
	}
}
