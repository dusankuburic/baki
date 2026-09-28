package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// roleBackend is a FakeBackend whose org listing carries a fixed roster:
// org-A has u-admin (admin), u-member and u-own (members), u-viewer (viewer)
// and u-guest (guest); u-collab is nobody's member (a per-flow collaborator).
type roleBackend struct {
	testutil.FakeBackend
	listErr error
}

func (b *roleBackend) ListOrgsForUser(_ context.Context, userID string) ([]*interfaces.Organisation, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	org := &interfaces.Organisation{ID: "org-A", Members: []interfaces.OrgMember{
		{UserID: "u-admin", Role: auth.RoleAdmin},
		{UserID: "u-member", Role: auth.RoleMember},
		{UserID: "u-own", Role: auth.RoleMember},
		{UserID: "u-none", Role: auth.RoleMember},
		{UserID: "u-viewer", Role: auth.RoleViewer},
		{UserID: "u-guest", Role: auth.RoleGuest},
	}}
	for _, m := range org.Members {
		if m.UserID == userID {
			return []*interfaces.Organisation{org}, nil
		}
	}
	return nil, nil
}

// orgKeyChatService is a ChatService on a factory whose org-A has a shared
// credential; members listed in memberKeys have their own. orgCalls counts
// org-store consultations.
func orgKeyChatService(memberKeys map[string]string, orgCalls *int, backend chatStore) *ChatService {
	registerFakeProvider()
	factory := ai.NewProviderFactory(func(scope, _ string) (string, error) { return memberKeys[scope], nil }, nil, nil, nil)
	factory.SetOrgKeyLookup(func(orgID, _ string) (string, error) {
		*orgCalls++
		if orgID == "org-A" {
			return "org-key", nil
		}
		return "", nil
	})
	return &ChatService{
		factory:  factory,
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 100}}},
		backend:  backend,
	}
}

// G7: prepareTurn runs an org flow on the organisation's shared credential
// when the member has none of their own, keeps the member's when they have
// one, and never consults the org for a personal flow.
func TestPrepareTurn_UsesOrgCredentialWhenMemberHasNone(t *testing.T) {
	orgCalls := 0
	svc := orgKeyChatService(map[string]string{"u-own": "own-key"}, &orgCalls, &roleBackend{})
	orgDoc := &models.FlowDocument{ID: "f1", OrganizationID: "org-A"}

	ctl := &streamCtl{ownerID: "u-none", orgID: "org-A"}
	ts, err := svc.prepareTurn(context.Background(), "u-none", orgDoc, models.ChatRequest{Provider: "fake"}, ctl)
	if err != nil {
		t.Fatalf("member without a key on an org flow: %v (want the org's credential)", err)
	}
	if ts.orgID != "org-A" || ts.provider == nil {
		t.Errorf("turn setup = %+v", ts)
	}
	if orgCalls != 1 {
		t.Errorf("org store consulted %d times, want 1", orgCalls)
	}
	if !ctl.sharedCredential.Load() {
		t.Error("the stream must know it runs on the org's shared credential")
	}

	orgCalls = 0
	ownCtl := &streamCtl{ownerID: "u-own", orgID: "org-A"}
	if _, err := svc.prepareTurn(context.Background(), "u-own", orgDoc, models.ChatRequest{Provider: "fake"}, ownCtl); err != nil {
		t.Fatalf("member with their own key: %v", err)
	}
	if orgCalls != 0 {
		t.Errorf("member's own key must win without consulting the org (consulted %d times)", orgCalls)
	}
	if ownCtl.sharedCredential.Load() {
		t.Error("the member's own key is not the shared credential")
	}

	orgCalls = 0
	personal := &models.FlowDocument{ID: "f2"}
	_, err = svc.prepareTurn(context.Background(), "u-none", personal, models.ChatRequest{Provider: "fake"}, &streamCtl{ownerID: "u-none"})
	if !errors.Is(err, ai.ErrKeyNotConfigured) {
		t.Errorf("personal flow without a key: err = %v, want ErrKeyNotConfigured", err)
	}
	if orgCalls != 0 {
		t.Errorf("personal flow consulted the org store %d times", orgCalls)
	}

	_, err = svc.prepareTurn(context.Background(), "u-none", &models.FlowDocument{ID: "f3", OrganizationID: "org-B"}, models.ChatRequest{Provider: "fake"}, &streamCtl{ownerID: "u-none", orgID: "org-B"})
	if !errors.Is(err, ai.ErrKeyNotConfigured) {
		t.Errorf("org without a credential: err = %v, want ErrKeyNotConfigured", err)
	}
}

// R-1: flow access is not org membership. Only the org's admins and members
// may run on its shared credential; a per-flow collaborator outside the org,
// a viewer and a guest need their own key (PROVIDER_NOT_CONFIGURED without
// one), and a failed membership lookup fails closed. The turn's budget and
// usage attribution stay on the flow's org either way.
func TestPrepareTurn_SharedCredentialOnlyForAdminsAndMembers(t *testing.T) {
	orgDoc := &models.FlowDocument{ID: "f1", OrganizationID: "org-A"}
	cases := []struct {
		caller     string
		ownKey     bool
		wantShared bool
		wantErr    bool
	}{
		{caller: "u-admin", wantShared: true},
		{caller: "u-member", wantShared: true},
		{caller: "u-viewer", wantErr: true},
		{caller: "u-guest", wantErr: true},
		{caller: "u-collab", wantErr: true},
		{caller: "u-collab", ownKey: true},
		{caller: "u-viewer", ownKey: true},
	}
	for _, tc := range cases {
		name := tc.caller
		if tc.ownKey {
			name += " with own key"
		}
		t.Run(name, func(t *testing.T) {
			keys := map[string]string{}
			if tc.ownKey {
				keys[tc.caller] = "own-key"
			}
			orgCalls := 0
			svc := orgKeyChatService(keys, &orgCalls, &roleBackend{})
			ctl := &streamCtl{ownerID: tc.caller, orgID: "org-A"}
			ts, err := svc.prepareTurn(context.Background(), tc.caller, orgDoc, models.ChatRequest{Provider: "fake"}, ctl)
			if tc.wantErr {
				if !errors.Is(err, ai.ErrKeyNotConfigured) {
					t.Fatalf("err = %v, want ErrKeyNotConfigured (own key only)", err)
				}
				if code, _ := ctl.classifyFailure(context.Background(), err); code != ChatErrProviderNotConfigured {
					t.Errorf("code = %q, want %s", code, ChatErrProviderNotConfigured)
				}
				if orgCalls != 0 {
					t.Errorf("the org's credential was consulted %d times for a caller who may not use it", orgCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareTurn: %v", err)
			}
			if ctl.sharedCredential.Load() != tc.wantShared {
				t.Errorf("shared credential = %v, want %v", ctl.sharedCredential.Load(), tc.wantShared)
			}
			if ts.orgID != "org-A" {
				t.Errorf("attribution org = %q, want the flow's org", ts.orgID)
			}
		})
	}

	t.Run("membership lookup failure fails closed", func(t *testing.T) {
		orgCalls := 0
		svc := orgKeyChatService(nil, &orgCalls, &roleBackend{listErr: errors.New("store down")})
		_, err := svc.prepareTurn(context.Background(), "u-member", orgDoc, models.ChatRequest{Provider: "fake"}, &streamCtl{ownerID: "u-member", orgID: "org-A"})
		if !errors.Is(err, ai.ErrKeyNotConfigured) || orgCalls != 0 {
			t.Errorf("err = %v, org consulted %d times; want own key only", err, orgCalls)
		}
	})
}

// PreviewContext applies the same rule as the turn: a non-member
// collaborator is refused (no key of their own), a member is not.
func TestPreviewContext_SharedCredentialRoleRule(t *testing.T) {
	orgCalls := 0
	svc := orgKeyChatService(nil, &orgCalls, &roleBackend{})
	doc := &models.FlowDocument{ID: "f1", OrganizationID: "org-A"}
	if _, err := svc.PreviewContext(context.Background(), "u-collab", doc, nil, models.ChatRequest{Provider: "fake"}); !errors.Is(err, ai.ErrKeyNotConfigured) {
		t.Errorf("collaborator preview: err = %v, want ErrKeyNotConfigured", err)
	}
	if orgCalls != 0 {
		t.Errorf("collaborator preview consulted the org's credential %d times", orgCalls)
	}
	if _, err := svc.PreviewContext(context.Background(), "u-member", doc, nil, models.ChatRequest{Provider: "fake"}); err != nil {
		t.Errorf("member preview: %v", err)
	}
}

// A credential failure on the org's shared credential tells the member to
// ask their org admin; the same failure on their own key keeps the
// Settings copy.
func TestClassifyFailure_SharedCredentialCopy(t *testing.T) {
	for _, err := range []error{ai.ErrApiKeyInvalid, ai.ErrProviderPermissionDenied, ai.ErrProviderConfigInvalid} {
		own := &streamCtl{}
		_, ownMsg := own.classifyFailure(context.Background(), err)
		shared := &streamCtl{}
		shared.sharedCredential.Store(true)
		code, msg := shared.classifyFailure(context.Background(), err)
		if !strings.Contains(msg, "ask your organisation admin to update the shared Azure AI Foundry configuration") {
			t.Errorf("%v on the shared credential: %q", err, msg)
		}
		if strings.Contains(ownMsg, "organisation admin") {
			t.Errorf("%v on the member's own key: %q must not blame the org", err, ownMsg)
		}
		if ownCode, _ := own.classifyFailure(context.Background(), err); ownCode != code {
			t.Errorf("%v: code changed with the credential (%q vs %q)", err, ownCode, code)
		}
	}
	shared := &streamCtl{}
	shared.sharedCredential.Store(true)
	if _, msg := shared.classifyFailure(context.Background(), ai.ErrRateLimited); strings.Contains(msg, "organisation admin") {
		t.Errorf("a rate limit is not the admin's to fix: %q", msg)
	}
}
