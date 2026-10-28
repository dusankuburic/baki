package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// Setup failures of a chat turn on the Azure AI Foundry model registry that
// the user cannot fix by choosing another model are never "the model was
// removed" (review R3), and a resource whose stored credentials cannot be
// decrypted asks for them again instead of "try again in a moment" (R5).

// A failed lookup of the caller's organisation role is a setup error — the organisation's
// entries could not be consulted — not ErrModelNotAvailable, the "removed" signal on
// which the client marks the model Removed and may re-pin the thread. The turn fails with
// the generic setup error and the preview with that error (a masked 500), whether the id
// is an organisation entry's or the server default, and nothing of the store's error
// reaches the client; the caller's own entries still answer (they come first).
func TestChatAzureRegistry_RoleLookupFailureIsTheGenericSetupError(t *testing.T) {
	ctx := context.Background()
	e := newAzChatEnv(t)
	orgScope := AzureScope{Scope: "admin", OrgID: "org-1"}
	oc := e.mustConn(t, orgScope, "team", azmKeyConfig("org-key"))
	shared := e.mustModel(t, orgScope, AzureModelInput{ConnectionID: oc.ID, Kind: "chat", Deployment: "gpt-4o"})
	built := e.scriptProviders(&itemsProvider{id: ai.AzureFoundryProviderID})
	orgFlow := &models.FlowDocument{ID: "f2", Name: "f2", OrganizationID: "org-1"}

	// The chat service's organisation store is down; the registry's own
	// directory is not.
	const secret = "pq: connection to 10.0.0.5 refused (password=hunter2)"
	e.backend.orgs = &azmOrgDir{orgs: map[string]*storageif.Organisation{}, err: errors.New(secret)}

	for _, c := range []struct{ name, model string }{
		{"the organisation's entry", shared.ModelID},
		{"the server default (no entry of the member's own)", ""},
	} {
		req := models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: c.model, UserMessage: "q", ConversationID: uuid.NewString()}
		if _, err := e.chat.PreviewContext(ctx, "u1", orgFlow, nil, req); !errors.Is(err, errProviderSetup) || errors.Is(err, ErrModelNotAvailable) {
			t.Errorf("%s: preview = %v, want the generic setup error", c.name, err)
		}
		id, err := e.chat.StreamChatMessage(ctx, "u1", orgFlow, nil, req)
		if err != nil {
			t.Fatalf("%s: create = %v, want the stream to report the setup error (not 400 MODEL_NOT_AVAILABLE)", c.name, err)
		}
		e.chat.BeginStream(ctx, id)
		snap := waitStreamFinished(t, e.chat, id).snapshot()
		if snap.Done || snap.ErrorCode != ChatErrProviderUnavailable || snap.Error != errProviderSetup.Error() || snap.ErrorShared {
			t.Errorf("%s: turn = %+v, want the generic setup error", c.name, snap)
		}
		if strings.Contains(snap.Error, "hunter2") || strings.Contains(snap.Error, "10.0.0.5") {
			t.Errorf("%s: the store's error reached the client: %q", c.name, snap.Error)
		}
	}
	if len(built()) != 0 {
		t.Errorf("providers built for %+v although the model could not be resolved", built())
	}

	// The member's own entry answers without the organisation, as does the
	// server default once there is one.
	mine := e.mustConn(t, azmUser, "mine", azmKeyConfig("k"))
	own := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: mine.ID, Kind: "chat", Deployment: "gpt-4.1"})
	for _, model := range []string{own.ModelID, ""} {
		snap := e.turn(t, "u1", orgFlow, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: model, UserMessage: "q", ConversationID: uuid.NewString()})
		if !snap.Done {
			t.Errorf("model %q: turn = %+v, want the member's own entry", model, snap)
		}
	}
	if refs := built(); len(refs) != 2 || refs[0] != (ai.AzureConnectionRef{ConnID: mine.ID}) || refs[1] != refs[0] {
		t.Errorf("providers built for %+v, want the member's own resource twice", refs)
	}

	// With the store back, an organisation entry is still refused to a
	// caller outside the organisation: not a member is "", not an error.
	e.backend.orgs = e.orgs
	if _, err := e.chat.StreamChatMessage(ctx, "u2", orgFlow, nil, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: shared.ModelID, UserMessage: "q"}); !errors.Is(err, ErrModelNotAvailable) {
		t.Errorf("non-member: create = %v, want ErrModelNotAvailable", err)
	}
}

// registryCredentialOrg reports a failed role lookup; credentialOrg, which
// picks the credential of every other provider's turn, still fails closed to
// the caller's own.
func TestRegistryCredentialOrg(t *testing.T) {
	ctx := context.Background()
	dir := &azmOrgDir{orgs: map[string]*storageif.Organisation{}}
	dir.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "u1": auth.RoleMember, "viewer": auth.RoleViewer})
	backend := &azChatBackend{orgs: dir}
	s := &ChatService{backend: backend}

	for _, c := range []struct{ orgID, scope, want string }{
		{"org-1", "admin", "org-1"},
		{"org-1", "u1", "org-1"},
		{"org-1", "viewer", ""},
		{"org-1", "u2", ""},
		{"", "u1", ""},
	} {
		if got, err := s.registryCredentialOrg(ctx, c.orgID, c.scope); got != c.want || err != nil {
			t.Errorf("registryCredentialOrg(%q, %q) = %q, %v; want %q", c.orgID, c.scope, got, err, c.want)
		}
	}

	dir.mu.Lock()
	dir.err = errors.New("db down")
	dir.mu.Unlock()
	if got, err := s.registryCredentialOrg(ctx, "org-1", "u1"); got != "" || err == nil || !strings.Contains(err.Error(), "db down") {
		t.Errorf("store down: registryCredentialOrg = %q, %v; want the store's error", got, err)
	}
	if got, err := s.registryCredentialOrg(ctx, "", "u1"); got != "" || err != nil {
		t.Errorf("personal flow, store down: registryCredentialOrg = %q, %v; want no lookup", got, err)
	}
	if got := s.credentialOrg(ctx, "org-1", "u1"); got != "" {
		t.Errorf("store down: credentialOrg = %q, want the caller's own credential (fail closed)", got)
	}
	// Local mode: no org, no error.
	if got, err := (&ChatService{}).registryCredentialOrg(ctx, "org-1", "u1"); got != "" || err != nil {
		t.Errorf("local mode: registryCredentialOrg = %q, %v", got, err)
	}
}

// A resource whose stored credentials the keystore cannot decrypt (the server's
// encryption key changed; the registry shows "unreadable") fails the turn as
// PROVIDER_NOT_CONFIGURED with copy that asks for the credentials again — the member's
// own resource in Settings → AI Accounts, an organisation's through its admin, with the
// shared flag — never the generic "try again in a moment". Nothing reaches Azure; the
// keystore's error stays in the log.
func TestChatAzureRegistry_UnreadableCredentials(t *testing.T) {
	e := newAzChatEnv(t)
	foundry := newAzChatFoundry(t)
	mine := e.mustConn(t, azmUser, "mine", azmKeyConfig("personal-key"))
	own := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: mine.ID, Kind: "chat", Deployment: "gpt-4o"})
	orgScope := AzureScope{Scope: "admin", OrgID: "org-1"}
	oc := e.mustConn(t, orgScope, "team", azmKeyConfig("org-key"))
	shared := e.mustModel(t, orgScope, AzureModelInput{ConnectionID: oc.ID, Kind: "chat", Deployment: "gpt-4o"})

	// The chat factory's keystores cannot decrypt either resource's document.
	decryptErr := fmt.Errorf("keystore: %w", storage.ErrSecretDecryptFailed)
	f := ai.NewProviderFactory(func(scope, provider string) (string, error) {
		if provider == ai.AzureConnectionKey(mine.ID) {
			return "", decryptErr
		}
		v, _ := e.secrets.Get(scope, provider)
		return v, nil
	}, nil, e.backend.SaveUsageMetric, nil)
	f.SetOrgKeyLookup(func(orgID, provider string) (string, error) {
		if provider == ai.AzureConnectionKey(oc.ID) {
			return "", decryptErr
		}
		return e.orgKeys.OrgKeyLookup(orgID, provider)
	})
	e.chat.factory = f

	snap := e.turn(t, "u1", &models.FlowDocument{ID: "f1", Name: "f1"}, models.ChatRequest{Provider: ai.AzureFoundryProviderID,
		Model: own.ModelID, UserMessage: "q", ConversationID: uuid.NewString()})
	if snap.Done || snap.ErrorCode != ChatErrProviderNotConfigured || snap.ErrorShared || snap.Error != azureUnreadableMsg {
		t.Errorf("own resource = %+v, want PROVIDER_NOT_CONFIGURED asking to enter the credentials again", snap)
	}

	snap = e.turn(t, "u1", &models.FlowDocument{ID: "f2", Name: "f2", OrganizationID: "org-1"}, models.ChatRequest{Provider: ai.AzureFoundryProviderID,
		Model: shared.ModelID, UserMessage: "q", ConversationID: uuid.NewString()})
	if snap.Done || snap.ErrorCode != ChatErrProviderNotConfigured || !snap.ErrorShared || snap.Error != azureUnreadableSharedMsg ||
		!strings.Contains(snap.Error, "ask your organisation admin") {
		t.Errorf("organisation's resource = %+v, want PROVIDER_NOT_CONFIGURED with the shared flag and the admin copy", snap)
	}
	if n := len(foundry.requests()); n != 0 {
		t.Errorf("the turns reached a resource %d time(s)", n)
	}
}

// The classification behind it: errAzureUnreadableCredentials survives
// providerSetupError (it is not the generic setup error) and is coded
// PROVIDER_NOT_CONFIGURED, with the admin's copy and the shared flag only on
// an organisation's credential — a resource with no credentials at all keeps
// its own copy.
func TestClassifyFailure_UnreadableAzureCredentials(t *testing.T) {
	cause := fmt.Errorf("%w: %w", errAzureUnreadableCredentials, fmt.Errorf("get azure-foundry/abc key: %w", storage.ErrSecretDecryptFailed))
	err := providerSetupError(context.Background(), "u1", ai.AzureFoundryProviderID, cause)
	if !errors.Is(err, errAzureUnreadableCredentials) || errors.Is(err, errProviderSetup) {
		t.Fatalf("providerSetupError = %v, want the unreadable-credentials error kept", err)
	}
	ctx := context.Background()
	own := &streamCtl{cancel: func() {}}
	if code, msg, shared := own.classifyFailureShared(ctx, err); code != ChatErrProviderNotConfigured || msg != azureUnreadableMsg || shared {
		t.Errorf("own resource = %q, %q, %v", code, msg, shared)
	}
	org := &streamCtl{cancel: func() {}}
	org.sharedCredential.Store(true)
	if code, msg, shared := org.classifyFailureShared(ctx, err); code != ChatErrProviderNotConfigured || msg != azureUnreadableSharedMsg || !shared {
		t.Errorf("organisation's resource = %q, %q, %v", code, msg, shared)
	}
	none := fmt.Errorf("%w: %w", errAzureNoCredentials, ai.ErrKeyNotConfigured)
	if _, msg, _ := org.classifyFailureShared(ctx, none); !strings.Contains(msg, "has no credentials") {
		t.Errorf("no credentials = %q, want its own copy", msg)
	}
}
