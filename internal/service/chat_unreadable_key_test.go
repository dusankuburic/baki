package service

// R-provider-18: a bare key the keystore cannot decrypt (the server's encryption key changed) fails the turn as
// PROVIDER_NOT_CONFIGURED with copy that asks for the key again, the member's own in Settings → AI Accounts and an
// organisation's through its admin with the shared flag, never the generic "try again in a moment" of
// PROVIDER_UNAVAILABLE. The keystore's text stays in the log.

import (
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/storage"
	"pad-core/models"

	"github.com/google/uuid"
)

func TestChat_UnreadableBareKeyAsksForItAgain(t *testing.T) {
	e := newAzChatEnv(t)
	decryptErr := fmt.Errorf("keystore: %w", storage.ErrSecretDecryptFailed)
	f := ai.NewProviderFactory(func(scope, provider string) (string, error) {
		if scope == "u1" && provider == "openai" {
			return "", decryptErr
		}
		return "", nil
	}, nil, e.backend.SaveUsageMetric, nil)
	f.SetOrgKeyLookup(func(orgID, provider string) (string, error) {
		if provider == "gemini" {
			return "", decryptErr
		}
		return "", nil
	})
	e.chat.factory = f

	snap := e.turn(t, "u1", &models.FlowDocument{ID: "f1", Name: "f1"},
		models.ChatRequest{Provider: "openai", UserMessage: "q", ConversationID: uuid.NewString()})
	if snap.Done || snap.ErrorCode != ChatErrProviderNotConfigured || snap.ErrorShared || snap.Error != keyUnreadableMsg {
		t.Errorf("own key = %+v, want PROVIDER_NOT_CONFIGURED asking to enter the key again", snap)
	}
	if strings.Contains(snap.Error, "keystore") {
		t.Errorf("the keystore's text reached the chat: %q", snap.Error)
	}

	snap = e.turn(t, "u1", &models.FlowDocument{ID: "f2", Name: "f2", OrganizationID: "org-1"},
		models.ChatRequest{Provider: "gemini", UserMessage: "q", ConversationID: uuid.NewString()})
	if snap.Done || snap.ErrorCode != ChatErrProviderNotConfigured || !snap.ErrorShared || snap.Error != keyUnreadableSharedMsg {
		t.Errorf("organisation's key = %+v, want PROVIDER_NOT_CONFIGURED with the shared flag and the admin copy", snap)
	}
}

// Review: a Copilot sign-in the keystore cannot decrypt is reported as one, not as "try again in a moment"; and an
// unreadable organisation key answers the HTTP routes with the admin's copy and the shared flag.
func TestUnreadable_CopilotTokenAndTheSharedHTTPCopy(t *testing.T) {
	e := newAzChatEnv(t)
	decryptErr := fmt.Errorf("keystore: %w", storage.ErrSecretDecryptFailed)
	e.chat.factory = ai.NewProviderFactory(func(scope, provider string) (string, error) {
		if provider == "copilot-oauth-token" {
			return "", decryptErr
		}
		return "", nil
	}, nil, e.backend.SaveUsageMetric, nil)
	snap := e.turn(t, "u1", &models.FlowDocument{ID: "f1", Name: "f1"},
		models.ChatRequest{Provider: "copilot", UserMessage: "q", ConversationID: uuid.NewString()})
	if snap.ErrorCode != ChatErrProviderNotConfigured || snap.Error != keyUnreadableMsg {
		t.Errorf("copilot = %+v, want PROVIDER_NOT_CONFIGURED asking to sign in again", snap)
	}

	shared, ok := apperr.As(SecretStoreError(&ai.OrgKeyError{Provider: "gemini", Err: decryptErr}))
	if !ok || shared.Code != ProviderCodeKeyUnreadable || shared.Details["shared"] != true || !strings.Contains(shared.Message, "organisation admin") {
		t.Errorf("org key = %+v, want KEY_UNREADABLE with the admin copy and shared", shared)
	}
	own, _ := apperr.As(SecretStoreError(fmt.Errorf("get gemini key: %w", decryptErr)))
	if own == nil || own.Details["shared"] != nil || strings.Contains(own.Message, "admin") {
		t.Errorf("own key = %+v, want the member's copy", own)
	}
}
