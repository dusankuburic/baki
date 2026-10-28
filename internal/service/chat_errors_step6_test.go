package service

// Step 6: the chat copy for the provider errors the transport and breaker work made precise.
//   - R-observability-7: an open circuit names the model and when to try again.
//   - R-provider-25: an OpenAI-family 403 is the provider refusing the account, in vendor-neutral words, and the
//     shared-key variant points at the organisation's admin; a quota 429 is the balance error.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
)

func TestClassifyFailure_Step6Copy(t *testing.T) {
	ctx := context.Background()
	own := &streamCtl{cancel: func() {}}

	code, msg := own.classifyFailure(ctx, fmt.Errorf("stream: %w", ai.ErrCircuitOpen))
	if code != ChatErrProviderUnavailable || !strings.Contains(msg, "temporarily unavailable") {
		t.Errorf("bare circuit open = %q, %q", code, msg)
	}

	// The typed error the breaker returns carries the model label and the cooldown.
	if got := circuitOpenMsg("gpt-4o", 12*time.Second); !strings.Contains(got, "gpt-4o") || !strings.Contains(got, "12 s") {
		t.Errorf("circuit open copy = %q, want the model and the remaining seconds", got)
	}
	if got := circuitOpenMsg("deployment", 500*time.Millisecond); !strings.Contains(got, "this deployment") || !strings.Contains(got, "1 s") {
		t.Errorf("alias circuit open copy = %q", got)
	}

	denied := ai.NewAccessDeniedError("Country, region, or territory not supported")
	code, msg = own.classifyFailure(ctx, denied)
	if code != ChatErrProviderPermission || strings.Contains(msg, "Foundry") || !strings.Contains(msg, "Country, region, or territory not supported") {
		t.Errorf("access denied = %q, %q; want PROVIDER_PERMISSION with the vendor's reason and no Azure copy", code, msg)
	}
	shared := &streamCtl{cancel: func() {}}
	shared.sharedCredential.Store(true)
	code, msg, isShared := shared.classifyFailureShared(ctx, denied)
	if code != ChatErrProviderPermission || !isShared || !strings.Contains(msg, "organisation admin") || strings.Contains(msg, "Azure") {
		t.Errorf("shared access denied = %q, %q, %v; want the admin copy without Azure", code, msg, isShared)
	}

	code, _ = own.classifyFailure(ctx, fmt.Errorf("%w: quota", ai.ErrInsufficientBalance))
	if code != ChatErrProviderBalance {
		t.Errorf("quota = %q, want %s", code, ChatErrProviderBalance)
	}
}
