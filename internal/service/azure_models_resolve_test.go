package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// azmResolveEnv: user u1 (a member of org-1) with a personal resource
// holding a chat, a Claude and an embedding model; org-1 with a resource and
// an agent-capable one.
type azmResolveEnv struct {
	*azmEnv
	personalChat, personalClaude, personalEmb AzureModelView
	orgChat, orgAgent                         AzureModelView
}

func newAzmResolveEnv(t *testing.T) *azmResolveEnv {
	e := &azmResolveEnv{azmEnv: newAzureModelsEnv(t)}
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "u1": auth.RoleMember})
	p := e.mustConn(t, azmUser, "mine", azmKeyConfig("k"))
	e.personalChat = e.mustModel(t, azmUser, AzureModelInput{ConnectionID: p.ID, Kind: "chat", Deployment: "gpt-4o", DisplayName: "My GPT"})
	e.personalClaude = e.mustModel(t, azmUser, AzureModelInput{ConnectionID: p.ID, Kind: "claude", Deployment: "claude-sonnet-4-6", DisplayName: "Docs writer", Purpose: "documentation"})
	e.personalEmb = e.mustModel(t, azmUser, AzureModelInput{ConnectionID: p.ID, Kind: "embedding", Deployment: "emb"})
	orgScope := AzureScope{Scope: "admin", OrgID: "org-1"}
	oc := e.mustConn(t, orgScope, "org", azmEntraConfig("s"))
	e.orgChat = e.mustModel(t, orgScope, AzureModelInput{ConnectionID: oc.ID, Kind: "chat", Deployment: "gpt-4.1", DisplayName: "Org GPT"})
	e.orgAgent = e.mustModel(t, orgScope, AzureModelInput{ConnectionID: oc.ID, Kind: "agent", Deployment: "reviewer", DisplayName: "Review bot", IsDefault: true})
	return e
}

func TestAzureModels_ResolveRegistryIDs(t *testing.T) {
	e := newAzmResolveEnv(t)
	ctx := context.Background()

	res, err := e.svc.ResolveChat(ctx, "u1", "", e.personalClaude.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResolvedModel != "claude:claude-sonnet-4-6" || res.RequestedModel != e.personalClaude.ModelID || res.ModelLabel != "Docs writer" ||
		res.Kind != "claude" || res.ConnectionRef.OrgID != "" || res.ConnectionRef.ConnID != e.personalClaude.ConnectionID || res.Shared || res.EntryID != e.personalClaude.ID {
		t.Errorf("personal = %+v", res)
	}
	// The full entry set of the resource, embedding included.
	if len(res.Entries) != 3 {
		t.Errorf("entries = %+v", res.Entries)
	}

	res, err = e.svc.ResolveChat(ctx, "u1", "org-1", e.orgAgent.ModelID)
	if err != nil || res.ResolvedModel != "agent:reviewer" || res.ConnectionRef.OrgID != "org-1" || !res.Shared || res.Kind != "agent" || len(res.Entries) != 2 {
		t.Errorf("org entry = %+v, %v", res, err)
	}
	notAvailable := map[string]struct{ credOrg, id string }{
		"lost access (no credential org)": {"", e.orgAgent.ModelID},
		"another organisation":            {"org-2", e.orgAgent.ModelID},
		"embedding model":                 {"", e.personalEmb.ModelID},
		"unknown entry":                   {"org-1", "m/zzzzzzzzzz"},
		"malformed entry":                 {"org-1", "m/NOPE"},
		"another user's entry":            {"", e.personalChat.ModelID},
	}
	for name, c := range notAvailable {
		scope := "u1"
		if name == "another user's entry" {
			scope = "u2"
		}
		if _, err := e.svc.ResolveChat(ctx, scope, c.credOrg, c.id); !errors.Is(err, ErrModelNotAvailable) {
			t.Errorf("%s: %v, want ErrModelNotAvailable", name, err)
		}
	}
}

// A registry failure is never a fallback — nor ErrModelNotAvailable.
func TestAzureModels_ResolveStoreFailureNeverFallsBack(t *testing.T) {
	e := newAzmResolveEnv(t)
	e.reg.FailOn("ListModels", errors.New("db down"))
	for _, id := range []string{"", e.personalChat.ModelID, "gpt-4o"} {
		res, err := e.svc.ResolveChat(context.Background(), "u1", "org-1", id)
		if err == nil || errors.Is(err, ErrModelNotAvailable) {
			t.Errorf("%q: %+v, %v; want the store's failure", id, res, err)
		}
	}
}

// Raw (pre-registry) ids: the caller's own "default" resource first — an
// alias, else the id itself on it, as the legacy document took any name; a
// caller without it gets the org's "default" aliases only; a raw id no org
// entry aliases is refused on the org credential.
func TestAzureModels_ResolveRawIDs(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "member": auth.RoleMember, "legacy-member": auth.RoleMember})
	_ = e.orgKeys.SaveOrg("org-1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"gpt-4.1","claudeDeployments":["claude-x"]`), "admin")
	_ = e.secrets.Save("legacy-member", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"mine"`))

	// The member without a "default" resource of their own reaches the org's
	// aliases — and nothing else on the org credential.
	res, err := e.svc.ResolveChat(ctx, "member", "org-1", "gpt-4.1")
	if err != nil || res.ConnectionRef.OrgID != "org-1" || res.ConnectionRef.ConnID != "default" || res.ResolvedModel != "gpt-4.1" || res.RequestedModel != "gpt-4.1" {
		t.Fatalf("org alias = %+v, %v", res, err)
	}
	if res, err := e.svc.ResolveChat(ctx, "member", "org-1", "claude:claude-x"); err != nil || res.Kind != "claude" || res.ConnectionRef.OrgID != "org-1" {
		t.Errorf("org claude alias = %+v, %v", res, err)
	}
	if _, err := e.svc.ResolveChat(ctx, "member", "org-1", "some-other-deployment"); !errors.Is(err, ErrModelNotAvailable) {
		t.Errorf("uncurated raw id on the org credential: %v", err)
	}
	// A member with a legacy document of their own: a legacy id never lands
	// on the admin's resource — their own "default" answers it, even for a
	// name only the org lists (it was never in the member's document).
	res, err = e.svc.ResolveChat(ctx, "legacy-member", "org-1", "gpt-4.1")
	if err != nil || res.ConnectionRef.OrgID != "" || res.ConnectionRef.ConnID != "default" || res.ResolvedModel != "gpt-4.1" || res.ModelLabel != "gpt-4.1" {
		t.Errorf("member-first = %+v, %v", res, err)
	}
	res, err = e.svc.ResolveChat(ctx, "legacy-member", "org-1", "MINE")
	if err != nil || res.EntryID == "" || res.ResolvedModel != "mine" || res.ConnectionRef.OrgID != "" {
		t.Errorf("own alias (case-insensitive) = %+v, %v", res, err)
	}
	if res, err := e.svc.ResolveChat(ctx, "legacy-member", "", "agent:helper"); err != nil || res.Kind != "agent" || res.ResolvedModel != "agent:helper" {
		t.Errorf("raw agent id through the legacy document = %+v, %v", res, err)
	}
	for _, bad := range []string{"a/b", "claude:", "agent:\x01"} {
		if _, err := e.svc.ResolveChat(ctx, "legacy-member", "", bad); !errors.Is(err, ErrModelNotAvailable) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Nobody else reaches anything.
	if _, err := e.svc.ResolveChat(ctx, "stranger", "", "gpt-4.1"); !errors.Is(err, ErrModelNotAvailable) {
		t.Errorf("stranger: %v", err)
	}
}

// model "" is the server default: the personal entry imported from the
// legacy chat deployment, else the first personal chat entry, else the
// org's default for members, else its first chat entry.
func TestAzureModels_ResolveServerDefault(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "u1": auth.RoleMember, "fresh": auth.RoleMember})
	// u1 kept a legacy document; its chat deployment sorts last on purpose.
	_ = e.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"zz-legacy","deployments":["aa-extra"]`))
	orgScope := AzureScope{Scope: "admin", OrgID: "org-1"}
	oc := e.mustConn(t, orgScope, "org", azmKeyConfig("ok"))
	firstOrg := e.mustModel(t, orgScope, AzureModelInput{ConnectionID: oc.ID, Kind: "chat", Deployment: "a-org", DisplayName: "A org"})
	orgDefault := e.mustModel(t, orgScope, AzureModelInput{ConnectionID: oc.ID, Kind: "chat", Deployment: "z-org", DisplayName: "Z org", IsDefault: true})
	e.mustModel(t, orgScope, AzureModelInput{ConnectionID: oc.ID, Kind: "claude", Deployment: "b-claude", DisplayName: "B claude"})
	e.mustModel(t, orgScope, AzureModelInput{ConnectionID: e.mustConn(t, orgScope, "agents", azmEntraConfig("s")).ID, Kind: "agent", Deployment: "a-agent", DisplayName: "0 agent"})

	def := func(scope string) string {
		t.Helper()
		res, err := e.svc.ResolveChat(ctx, scope, "org-1", "")
		if errors.Is(err, ErrModelNotAvailable) {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		if res.RequestedModel != "m/"+res.EntryID {
			t.Errorf("requested = %q for entry %q", res.RequestedModel, res.EntryID)
		}
		return res.EntryID
	}
	if got := def("fresh"); got != orgDefault.ID {
		t.Errorf("no personal models: %q, want the org default %q", got, orgDefault.ID)
	}
	if _, err := e.svc.UpdateModel(ctx, orgScope, orgDefault.ID, AzureModelPatch{IsDefault: new(bool)}); err != nil {
		t.Fatal(err)
	}
	if got := def("fresh"); got != firstOrg.ID {
		// Chat and Claude entries in display order, before any agent.
		t.Errorf("no org default: %q, want the first org chat entry %q", got, firstOrg.ID)
	}
	// A personal model wins (member-first), the legacy chat deployment first.
	u1 := storageif.AzureUserOwner("u1")
	if got := def("u1"); got != legacyAzureEntryID(u1, "chat", "zz-legacy") {
		t.Errorf("legacy chat deployment first: %q", got)
	}
	if err := e.svc.DeleteModel(ctx, azmUser, legacyAzureEntryID(u1, "chat", "zz-legacy"), false); err != nil {
		t.Fatal(err)
	}
	if got := def("u1"); got != legacyAzureEntryID(u1, "chat", "aa-extra") {
		t.Errorf("then the first personal chat entry: %q", got)
	}
	// Only usable entries: a resource without credentials is skipped.
	_ = e.secrets.Delete("u1", "azure-foundry")
	if got := def("u1"); got != firstOrg.ID {
		t.Errorf("personal resource without credentials: %q, want the org's %q", got, firstOrg.ID)
	}
	// Nothing at all.
	if _, err := e.svc.ResolveChat(ctx, "stranger", "", ""); !errors.Is(err, ErrModelNotAvailable) {
		t.Errorf("no default: %v", err)
	}
}
