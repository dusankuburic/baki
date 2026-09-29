package ai

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestEntraRejectionReason(t *testing.T) {
	refused := &entraTokenError{rejected: true, msg: "Microsoft Entra ID refused the sign-in: the client secret has expired — create a new secret (AADSTS7000222 invalid_client)"}
	wrapped := fmt.Errorf("get azure ai foundry token: %w", refused)
	reason, ok := EntraRejectionReason(wrapped)
	if !ok || !strings.Contains(reason, "client secret has expired") || !strings.Contains(reason, "AADSTS7000222") {
		t.Errorf("EntraRejectionReason = %q, %v; want the refusal's own one-line message", reason, ok)
	}
	if !errors.Is(wrapped, ErrEntraCredentialsRejected) {
		t.Error("a refused sign-in must still match ErrEntraCredentialsRejected")
	}

	unavailable := &entraTokenError{msg: "Microsoft Entra ID is temporarily unavailable — try again shortly"}
	if _, ok := EntraRejectionReason(unavailable); ok {
		t.Error("an unavailable token endpoint is not a credential refusal")
	}
	if _, ok := EntraRejectionReason(ErrEntraCredentialsRejected); ok {
		t.Error("the bare sentinel carries no specific reason")
	}
	if _, ok := EntraRejectionReason(nil); ok {
		t.Error("nil is not a refusal")
	}
}
