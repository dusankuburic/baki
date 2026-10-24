package service

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// TestPreviewContext_UserMessagePastedPADIsMasked (R-security-c14): the preview shows the question as the request
// build sends it, so a pasted PAD line whose literal the regex pass alone lets through is masked there too.
func TestPreviewContext_UserMessagePastedPADIsMasked(t *testing.T) {
	doc := parseCtxBlockDoc(t, ctxBlockSrc)
	svc := newCtxBlockChatService(t, "freetext-preview", &testutil.FakeProvider{})
	res, err := svc.PreviewContext(context.Background(), "u1", doc, nil, models.ChatRequest{
		Provider: "freetext-preview", Model: "fake-model",
		UserMessage: "The login step fails. The flow sets it like this:\nSET Password TO $'''Winter#2024-pv'''",
	})
	if err != nil {
		t.Fatalf("PreviewContext: %v", err)
	}
	if strings.Contains(res.UserMessage, "Winter#2024-pv") || !strings.Contains(res.UserMessage, "SET Password TO $'''[REDACTED]'''") {
		t.Errorf("preview user message = %q, want the pasted literal masked", res.UserMessage)
	}
	if !strings.Contains(res.UserMessage, "The login step fails.") {
		t.Errorf("preview user message = %q, want the prose kept", res.UserMessage)
	}
}
