package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
)

// The model registry's own credential writes — a resource created, its credential re-entered, the resource removed —
// are announced as CredentialChanged, so the caches derived from credentials hear them: the knowledge base's
// memoised embedders (rag.embedders) would otherwise keep embedding with a revoked org Foundry key until their TTL.
// A change that leaves the credential alone (a rename) announces nothing.
func TestAzureModels_CredentialWritesAreAnnounced(t *testing.T) {
	bus := eventbus.New(nil, 0)
	rec := eventbus.NewRecorder(bus)
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.Events = bus })
	ctx := context.Background()
	org := AzureScope{Scope: "admin-a", OrgID: "org-1"}
	orgScope := ai.OrgScope("org-1")

	announced := func() []events.CredentialChanged {
		t.Helper()
		got := eventbus.Recorded[events.CredentialChanged](rec)
		rec.Reset()
		return got
	}
	isOrgFoundry := func(got []events.CredentialChanged) bool {
		return len(got) == 1 && got[0].Scope == orgScope && got[0].OrgID == "org-1" && got[0].ProviderID == ai.AzureFoundryProviderID
	}

	c := e.mustConn(t, org, "shared", azmKeyConfig("k1"))
	if got := announced(); !isOrgFoundry(got) {
		t.Errorf("create announced %+v, want one change of org:org-1's azure-foundry", got)
	}
	name := "renamed"
	if _, err := e.svc.UpdateConnection(ctx, org, c.ID, AzureConnectionPatch{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if got := announced(); len(got) != 0 {
		t.Errorf("a rename announced %+v, want nothing", got)
	}
	rotated := azmKeyConfig("k2")
	v, err := e.svc.UpdateConnection(ctx, org, c.ID, AzureConnectionPatch{Config: &rotated, CredentialUpdatedAt: c.CredentialUpdatedAt})
	if err != nil {
		t.Fatalf("re-enter the credential: %v", err)
	}
	if got := announced(); !isOrgFoundry(got) {
		t.Errorf("a re-entered credential announced %+v", got)
	}
	_ = v
	if _, err := e.svc.DeleteConnection(ctx, org, c.ID, AzureDeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := announced(); !isOrgFoundry(got) {
		t.Errorf("a removed resource announced %+v", got)
	}

	e.mustConn(t, azmUser, "mine", azmKeyConfig("k"))
	if got := announced(); len(got) != 1 || got[0].Scope != azmUser.Scope || got[0].OrgID != "" {
		t.Errorf("a personal resource announced %+v, want one change of u1", got)
	}
}
