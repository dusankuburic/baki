package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-core/models"
)

// azureProviderEntry returns the azure-foundry entry of a provider listing.
func azureProviderEntry(t *testing.T, list []models.ProviderInfo) models.ProviderInfo {
	t.Helper()
	for _, p := range list {
		if p.ID == ai.AzureFoundryProviderID {
			return p
		}
	}
	t.Fatalf("no %s entry in %+v", ai.AzureFoundryProviderID, list)
	return models.ProviderInfo{}
}

// With the model registry wired, the Azure AI Foundry entry of the provider
// listing is built from it — the caller's registered chat, Claude and agent
// entries plus the flow organisation's shared ones, "m/" ids, the server
// default, configured/shared as for every other provider — without a single
// request to Azure. Every other provider is listed as before.
func TestListProviders_AzureFoundryFromTheRegistry(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "member": auth.RoleMember, "u1": auth.RoleMember})
	svc := NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), e.factory, e.secrets)
	svc.SetOrgKeyStore(e.orgKeys)
	svc.SetAzureModels(e.svc)
	rt, _ := configureAzureWithFake(t, http.StatusOK, `{}`)

	c := e.mustConn(t, azmUser, "contoso-prod", azmKeyConfig("k"))
	gpt := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-4o", DisplayName: "GPT-4o"})
	docs := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "claude", Deployment: "claude-sonnet-4-6",
		DisplayName: "Docs writer", Purpose: "documentation", ContextWindow: 200000})
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "embedding", Deployment: "emb"})
	orgScope := AzureScope{Scope: "admin", OrgID: "org-1"}
	oc := e.mustConn(t, orgScope, "team", azmKeyConfig("org-key"))
	shared := e.mustModel(t, orgScope, AzureModelInput{ConnectionID: oc.ID, Kind: "chat", Deployment: "gpt-4.1", DisplayName: "Team GPT", IsDefault: true})

	list, err := svc.ListProviders(ctx, "u1", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	az := azureProviderEntry(t, list)
	if !az.Configured || az.Shared || az.SuggestDiscovery || az.Name != "Azure AI Foundry" || az.AuthType != "api_key" {
		t.Errorf("entry = %+v", az)
	}
	var ids []string
	for _, m := range az.Models {
		ids = append(ids, m.ID)
		if !strings.HasPrefix(m.ID, "m/") || m.Kind == "embedding" {
			t.Errorf("listed %+v", m)
		}
	}
	// Purpose order (general, then documentation), personal before shared.
	if got, want := strings.Join(ids, ","), strings.Join([]string{gpt.ModelID, shared.ModelID, docs.ModelID}, ","); got != want {
		t.Errorf("models = %s, want %s", got, want)
	}
	// The server default: the caller's first chat entry (member-first) —
	// not the organisation's default for members.
	if az.DefaultModel != gpt.ModelID {
		t.Errorf("default = %q, want %q", az.DefaultModel, gpt.ModelID)
	}
	if d := azmDetail(t, az.Models, docs.ModelID); d.ContextLimit != 200000 || d.Kind != "claude" || d.DisplayName != "Docs writer" || d.Shared {
		t.Errorf("claude entry = %+v", d)
	}
	if s := azmDetail(t, az.Models, shared.ModelID); !s.Shared || !s.IsOrgDefault || s.IsDefault || s.OrgName != "Contoso" {
		t.Errorf("shared entry = %+v", s)
	}
	if n := rt.calls.Load(); n != 0 {
		t.Errorf("the listing made %d request(s) to Azure", n)
	}
	var sawCatalogue bool
	for _, p := range list {
		sawCatalogue = sawCatalogue || (p.ID == "openai" && !p.Configured && len(p.Models) > 0)
	}
	if !sawCatalogue {
		t.Error("the other providers are no longer listed from their catalogue")
	}

	// A member with no resource of their own runs on the organisation's:
	// configured and shared, the organisation's default for members.
	az = azureProviderEntry(t, mustListProviders(t, svc, "member", "org-1"))
	if !az.Configured || !az.Shared || len(az.Models) != 1 || az.Models[0].ID != shared.ModelID || az.DefaultModel != shared.ModelID {
		t.Errorf("member with the org = %+v", az)
	}
	// Without the organisation (a personal flow, or a viewer): nothing.
	az = azureProviderEntry(t, mustListProviders(t, svc, "member", ""))
	if az.Configured || az.Shared || len(az.Models) != 0 || az.DefaultModel != "" {
		t.Errorf("member without the org = %+v", az)
	}

	// The migrated Entra ID + project configuration suggests discovering
	// what the project holds.
	if err := e.secrets.Save("u3", "azure-foundry", `{"endpoint":"https://`+azureTestHost+`","projectEndpoint":"https://`+azureTestHost+`/api/projects/p1",`+
		`"auth":{"mode":"entra","tenantId":"`+azmTenant+`","clientId":"`+azmClient+`","clientSecret":"s"}}`); err != nil {
		t.Fatal(err)
	}
	az = azureProviderEntry(t, mustListProviders(t, svc, "u3", ""))
	if !az.Configured || !az.SuggestDiscovery || len(az.Models) != 0 || az.DefaultModel != "" {
		t.Errorf("migrated Entra ID + project resource = %+v", az)
	}
	if n := rt.calls.Load(); n != 0 {
		t.Errorf("the listings made %d request(s) to Azure", n)
	}

	// A registry that cannot be read fails the listing: an Azure entry
	// without its models would read as every model removed.
	e.reg.FailOn("ListModels", errors.New("db down"))
	if _, err := svc.ListProviders(ctx, "u1", ""); err == nil {
		t.Error("a registry failure listed Azure AI Foundry anyway")
	}
}

func mustListProviders(t *testing.T, svc *ProviderService, scope, orgID string) []models.ProviderInfo {
	t.Helper()
	list, err := svc.ListProviders(context.Background(), scope, orgID)
	if err != nil {
		t.Fatalf("ListProviders(%s, %q): %v", scope, orgID, err)
	}
	return list
}
