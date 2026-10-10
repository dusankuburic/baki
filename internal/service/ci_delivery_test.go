package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/platform/secret"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

func ciTokenActor(userID, tokenID string, scopes ...string) *auth.Actor {
	return &auth.Actor{Kind: auth.ActorToken, UserID: userID, Role: auth.RoleMember, TokenID: tokenID, Scopes: scopes}
}

// Only a personal API token carrying the ci scope authenticates, and only where accounts exist; every refusal is
// the one typed 401.
func TestCIService_AuthenticateToken(t *testing.T) {
	f := newCIFixture(t, CIOptions{OrgCredentials: true})
	for name, actor := range map[string]*auth.Actor{
		"no token":              nil,
		"a session":             {Kind: auth.ActorUser, UserID: "admin", Role: auth.RoleMember},
		"an unscoped token":     ciTokenActor("admin", "tok"),
		"a read+write token":    ciTokenActor("admin", "tok", auth.ScopeRead, auth.ScopeWrite),
		"a ci token without id": ciTokenActor("admin", "", auth.ScopeCI),
		"an integration":        auth.IntegrationActor("org-a", "key"),
	} {
		if c, err := f.svc.AuthenticateToken(actor); !errors.Is(err, ErrCITokenUnauthenticated) {
			t.Errorf("%s: %+v, %v; want ErrCITokenUnauthenticated", name, c, err)
		}
	}
	c, err := f.svc.AuthenticateToken(ciTokenActor("admin", "tok", auth.ScopeRead, auth.ScopeCI))
	if err != nil || c.Method != CIAuthToken || c.OrgID != "" || c.CredentialID != "" || c.Deprecated {
		t.Fatalf("a ci token: %+v, %v", c, err)
	}
	if a := c.Actor(); a.Kind != auth.ActorToken || a.UserID != "admin" || a.TokenID != "tok" {
		t.Errorf("the caller acts as %+v, want the token", a)
	}

	// Without accounts there are no tokens to admit, whatever the bearer claims.
	noAccounts := newCIFixture(t, CIOptions{GlobalSecret: secret.New("global-secret")})
	if _, err := noAccounts.svc.AuthenticateToken(ciTokenActor("admin", "tok", auth.ScopeCI)); !errors.Is(err, ErrCITokenUnauthenticated) {
		t.Errorf("without accounts: %v", err)
	}
}

// A token reaches the flows its user can read, by the authorization rule of every other route: the user's own and
// their organisations' flows; another user's is a refusal the webhook answers like an unknown flow.
func TestCIService_ResolveFlow_Token(t *testing.T) {
	f := newCIFixture(t, CIOptions{OrgCredentials: true})
	if err := f.store.SaveFlow(f.ctx, &storageif.FlowDocument{ID: "flow-other", Name: "Other", OwnerID: "u-other", Content: json.RawMessage(`{"name":"Other"}`)}); err != nil {
		t.Fatal(err)
	}
	caller, err := f.svc.AuthenticateToken(ciTokenActor("admin", "tok", auth.ScopeCI))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"flow-personal", "flow-a", "flow-b"} {
		if doc, err := f.svc.ResolveFlow(f.ctx, caller, id); err != nil || doc.ID != id {
			t.Errorf("%s: %+v, %v", id, doc, err)
		}
	}
	if _, err := f.svc.ResolveFlow(f.ctx, caller, "flow-other"); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("another user's flow: %v, want a refusal", err)
	}
	if _, err := f.svc.ResolveFlow(f.ctx, caller, "flow-missing"); !errors.Is(err, storageif.ErrNotFound) {
		t.Errorf("an unknown flow: %v, want ErrNotFound", err)
	}
	// A collaborator reaches a personal flow shared with them, but not an organisation's flow unless a member: a CI
	// run analyses with the organisation's custom rules, which a non-member's view lacks, and records onto its
	// dashboard.
	if err := f.store.CreateUser(f.ctx, &storageif.User{ID: "u-collab", Email: "collab@example.com", Password: "hash"}); err != nil {
		t.Fatal(err)
	}
	for _, flow := range []string{"flow-personal", "flow-a"} {
		if err := f.store.AddCollaborator(f.ctx, flow, &storageif.Collaborator{UserID: "u-collab", Permission: "editor"}); err != nil {
			t.Fatal(err)
		}
	}
	collab, err := f.svc.AuthenticateToken(ciTokenActor("u-collab", "tok-collab", auth.ScopeCI))
	if err != nil {
		t.Fatal(err)
	}
	if doc, err := f.svc.ResolveFlow(f.ctx, collab, "flow-personal"); err != nil || doc.ID != "flow-personal" {
		t.Errorf("a collaborator's personal flow: %+v, %v", doc, err)
	}
	if _, err := f.svc.ResolveFlow(f.ctx, collab, "flow-a"); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("an organisation's flow shared with a non-member: %v, want a refusal", err)
	}
	// Without authorization wired, a token reaches nothing: never the deployment secret's unscoped lookup.
	bare := NewCIService(f.store, NewCloudDocumentProvider(f.store), nil, CIOptions{OrgCredentials: true})
	if _, err := bare.ResolveFlow(f.ctx, caller, "flow-personal"); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("without authz: %v, want a refusal", err)
	}
}

// The delivery id is recorded per key: the same id from the same key is a replay, from another key it is not; the
// credential is one key whichever way it authenticated. A missing id goes on, a malformed one is a 400.
func TestCIService_ClaimDelivery(t *testing.T) {
	f := newCIFixture(t, CIOptions{OrgCredentials: true, GlobalSecret: secret.New("global-secret")})
	byKeyID := &CICaller{Method: CIAuthKeyID, OrgID: "org-a", CredentialID: "key-1"}
	byFlow := &CICaller{Method: CIAuthFlowOrg, OrgID: "org-a", CredentialID: "key-1"}
	otherKey := &CICaller{Method: CIAuthKeyID, OrgID: "org-a", CredentialID: "key-2"}
	secret := &CICaller{Method: CIAuthGlobal}
	token, err := f.svc.AuthenticateToken(ciTokenActor("admin", "tok", auth.ScopeCI))
	if err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef"

	if err := f.svc.ClaimDelivery(f.ctx, byKeyID, id); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	for name, c := range map[string]*CICaller{"the same credential": byKeyID, "the credential through the flow": byFlow} {
		err := f.svc.ClaimDelivery(f.ctx, c, id)
		if ae, ok := apperr.As(err); !ok || !errors.Is(err, ErrCIReplayed) || ae.Kind.Status() != http.StatusConflict || ae.Code != "CI_REPLAYED" {
			t.Errorf("%s: %v, want ErrCIReplayed", name, err)
		}
	}
	for name, c := range map[string]*CICaller{"another credential": otherKey, "the deployment secret": secret, "a token": token} {
		if err := f.svc.ClaimDelivery(f.ctx, c, id); err != nil {
			t.Errorf("%s: %v, want its own record", name, err)
		}
	}
	for range 2 {
		if err := f.svc.ClaimDelivery(f.ctx, byKeyID, ""); err != nil {
			t.Errorf("no delivery id: %v", err)
		}
	}
	for _, bad := range []string{"0123456789abcde", "0123456789abcdef+", "0123456789abcdef\x00"} {
		err := f.svc.ClaimDelivery(f.ctx, byKeyID, bad)
		if ae, ok := apperr.As(err); !ok || ae.Kind.Status() != http.StatusBadRequest {
			t.Errorf("deliveryId %q: %v, want a 400", bad, err)
		}
	}

	failing := NewCIService(&deliveryFailingStore{FakeBackend: testutil.NewFakeBackend()}, nil, nil, CIOptions{OrgCredentials: true})
	if err := failing.ClaimDelivery(f.ctx, byKeyID, id); err == nil || errors.Is(err, ErrCIReplayed) {
		t.Errorf("a store failure: %v, want an untyped error (the call must not run)", err)
	} else if _, typed := apperr.As(err); typed {
		t.Errorf("a store failure is typed: %v", err)
	}
}

type deliveryFailingStore struct{ *testutil.FakeBackend }

func (deliveryFailingStore) RecordCIDelivery(context.Context, string, string) (bool, error) {
	return false, errors.New("connection reset")
}
