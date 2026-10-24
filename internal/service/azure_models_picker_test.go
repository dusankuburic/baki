package service

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

func azmDetail(t *testing.T, list []models.ModelDetail, id string) models.ModelDetail {
	t.Helper()
	for _, m := range list {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("%s not listed in %+v", id, list)
	return models.ModelDetail{}
}

// The picker is driven by the registry: every chat, Claude and agent entry
// of the caller and of the flow's organisation is listed — whatever a live
// listing says — with its purpose, kind, status and window; embedding
// entries never are. Aliases ride only on "default"-resource entries, an
// organisation's only for a member without a "default" of their own.
func TestAzureModels_PickerListing(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "member": auth.RoleMember, "legacy": auth.RoleMember})
	_ = e.orgKeys.SaveOrg("org-1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"org-chat"`), "admin")
	_ = e.secrets.Save("legacy", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"gpt-4o","embeddingDeployment":"emb"`))
	legacy := AzureScope{Scope: "legacy"}
	extra := e.mustConn(t, legacy, "extra", azmEntraConfig("s"))
	agent := e.mustModel(t, legacy, AzureModelInput{ConnectionID: extra.ID, Kind: "agent", Deployment: "helper", DisplayName: "Helper", Purpose: "review"})
	small := e.mustModel(t, legacy, AzureModelInput{ConnectionID: extra.ID, Kind: "chat", Deployment: "distilled", DisplayName: "Distilled",
		Purpose: "documentation", Description: "Writes docs", ContextWindow: 16384})
	broken := e.mustConn(t, legacy, "broken", azmKeyConfig("k"))
	gone := e.mustModel(t, legacy, AzureModelInput{ConnectionID: broken.ID, Kind: "chat", Deployment: "gone"})
	_ = e.secrets.Delete("legacy", "azure-foundry/"+broken.ID)

	out, err := e.svc.PickerListing(ctx, "legacy", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Configured || out.Shared {
		t.Errorf("configured/shared = %v/%v", out.Configured, out.Shared)
	}
	var ids []string
	for _, m := range out.Models {
		ids = append(ids, m.DisplayName)
	}
	// Purpose order (general, documentation, review), personal before shared.
	if got := strings.Join(ids, ","); got != "gone,gpt-4o,org-chat,Distilled,Helper" {
		t.Errorf("order = %s", got)
	}
	gpt := azmDetail(t, out.Models, "m/"+legacyAzureEntryID(e.svc.userOwner("legacy"), "chat", "gpt-4o"))
	if gpt.Kind != "chat" || gpt.Shared || gpt.ConnectionID != "default" || gpt.Status != AzureStatusReady || strings.Join(gpt.Aliases, ",") != "gpt-4o" ||
		!gpt.IsDefault || gpt.ContextLimit != 128000 || !gpt.SupportsTools {
		t.Errorf("gpt-4o = %+v", gpt)
	}
	if out.DefaultModel != gpt.ID {
		t.Errorf("default = %q, want the imported chat deployment %q", out.DefaultModel, gpt.ID)
	}
	d := azmDetail(t, out.Models, small.ModelID)
	if d.ContextLimit != 16384 || d.Purpose != "documentation" || d.Description != "Writes docs" || d.ConnectionName != "extra" || len(d.Aliases) != 0 {
		t.Errorf("distilled = %+v", d)
	}
	if a := azmDetail(t, out.Models, agent.ModelID); a.Kind != "agent" || a.Status != AzureStatusReady || a.ContextLimit != 128000 {
		t.Errorf("agent = %+v", a)
	}
	if g := azmDetail(t, out.Models, gone.ModelID); g.Status != AzureStatusNeedsCredentials || g.StatusReason == "" {
		t.Errorf("entry on a resource without credentials = %+v", g)
	}
	orgChat := azmDetail(t, out.Models, "m/"+legacyAzureEntryID(storageif.AzureOrgOwner("org-1"), "chat", "org-chat"))
	if !orgChat.Shared || !orgChat.IsOrgDefault || orgChat.IsDefault || len(orgChat.Aliases) != 0 {
		t.Errorf("org entry for a member with a default resource of their own = %+v", orgChat)
	}
	if orgChat.OrgID != "org-1" || orgChat.OrgName != "Contoso" || gpt.OrgID != "" || gpt.OrgName != "" {
		t.Errorf("org names: shared %q/%q, personal %q/%q", orgChat.OrgID, orgChat.OrgName, gpt.OrgID, gpt.OrgName)
	}
	if out.SuggestDiscovery {
		t.Error("an API-key migrated resource cannot suggest discovery")
	}
	for _, m := range out.Models {
		if m.Kind == "embedding" || m.Kind == "" {
			t.Errorf("listed %+v", m)
		}
	}

	// A member without a "default" resource: the org's aliases, and its
	// default for members as the server default.
	out, err = e.svc.PickerListing(ctx, "member", "org-1")
	if err != nil || out.Configured || !out.Shared || len(out.Models) != 1 {
		t.Fatalf("member listing = %+v, %v", out, err)
	}
	if m := out.Models[0]; strings.Join(m.Aliases, ",") != "org-chat" || !m.IsDefault || out.DefaultModel != m.ID {
		t.Errorf("member's org entry = %+v (default %q)", m, out.DefaultModel)
	}
	// No org, no models.
	if out, err := e.svc.PickerListing(ctx, "member", ""); err != nil || out.Configured || out.Shared || len(out.Models) != 0 || out.DefaultModel != "" {
		t.Errorf("without the org = %+v, %v", out, err)
	}
}

// The read paths never wait on the network: the listings, the resources and
// the picker are built from the registry and the process caches; a stale
// project listing is only TRIGGERED in the background.
func TestAzureModels_ReadPathsMakeNoNetworkCalls(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	en := e.mustConn(t, azmUser, "entra", azmEntraConfig("s"))
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: en.ID, Kind: "chat", Deployment: "gpt-4o"})
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: en.ID, Kind: "agent", Deployment: "helper"})
	keyed := e.mustConn(t, azmUser, "keyed", azmKeyConfig("k"))
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: keyed.ID, Kind: "chat", Deployment: "gpt-4.1"})
	rt, _ := configureAzureWithFake(t, http.StatusOK, `{}`)
	var mu sync.Mutex
	var triggered []string
	e.svc.refresher = newAzureListingRefresher(func(_ context.Context, raw string, _ []ai.AzureRegisteredModel) error {
		mu.Lock()
		defer mu.Unlock()
		triggered = append(triggered, raw)
		return nil
	})
	if _, err := e.svc.ListConnections(ctx, azmUser); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ListModels(ctx, azmUser); err != nil {
		t.Fatal(err)
	}
	out, err := e.svc.PickerListing(ctx, "u1", "")
	if err != nil || len(out.Models) != 3 {
		t.Fatalf("picker = %+v, %v", out, err)
	}
	e.svc.refresher.wg.Wait()
	if n := rt.calls.Load(); n != 0 {
		t.Errorf("the read paths made %d request(s)", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(triggered) == 0 {
		t.Fatal("no background refresh was triggered for the Entra ID + project resource")
	}
	for _, raw := range triggered {
		if !strings.Contains(raw, `"mode":"entra"`) {
			t.Errorf("an API-key resource was refreshed: %s", raw)
		}
	}
}

// At most four listings refresh at once, one per resource at a time; the
// rest wait in the queue the workers drain — none is starved by the order
// the reads list resources in.
func TestAzureModels_RefreshFanOutAtMostFour(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		c := e.mustConn(t, azmUser, "r"+strings.Repeat("x", i), azmEntraConfig("s"))
		e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-4o"})
	}
	release := make(chan struct{})
	var running, peak, total atomic.Int32
	var seenMu sync.Mutex
	seen := map[string]int{}
	e.svc.refresher = newAzureListingRefresher(func(_ context.Context, _ string, entries []ai.AzureRegisteredModel) error {
		seenMu.Lock()
		seen[entries[0].EntryID]++
		seenMu.Unlock()
		n := running.Add(1)
		total.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return nil
	})
	for i := 0; i < 3; i++ {
		if _, err := e.svc.PickerListing(ctx, "u1", ""); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for running.Load() < azureRefreshParallel && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // a fifth would have started by now
	if got := running.Load(); got != azureRefreshParallel {
		t.Errorf("running = %d, want %d", got, azureRefreshParallel)
	}
	close(release)
	e.svc.refresher.wg.Wait()
	if p := peak.Load(); p > azureRefreshParallel {
		t.Errorf("peak concurrency = %d, want at most %d", p, azureRefreshParallel)
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != 6 || total.Load() != 6 {
		t.Errorf("refreshed %v (%d calls), want each of the 6 resources once despite three reads", seen, total.Load())
	}
}

// The picker's "your project has models you haven't added" prompt: only for
// the migrated resource, when it can list its project, until dismissed, and
// never on a read-only registry.
func TestAzureModels_PickerSuggestDiscovery(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	_ = e.secrets.Save("u1", "azure-foundry", `{"endpoint":"https://`+azureTestHost+`","projectEndpoint":"https://`+azureTestHost+`/api/projects/p1",`+
		`"auth":{"mode":"entra","tenantId":"`+azmTenant+`","clientId":"`+azmClient+`","clientSecret":"s"}}`)
	out, err := e.svc.PickerListing(ctx, "u1", "")
	if err != nil || !out.SuggestDiscovery {
		t.Fatalf("migrated Entra ID + project resource: %+v, %v", out, err)
	}
	e.reg.SetStatus(storageif.AzureRegistryStatus{State: storageif.AzureRegistryReadOnly})
	if out, _ := e.svc.PickerListing(ctx, "u1", ""); out.SuggestDiscovery {
		t.Error("a read-only registry cannot take the models")
	}
	e.reg.SetStatus(storageif.AzureRegistryStatus{State: storageif.AzureRegistryOK})
	dismiss := true
	if _, err := e.svc.UpdateConnection(ctx, azmUser, "default", AzureConnectionPatch{DismissDiscovery: &dismiss}); err != nil {
		t.Fatal(err)
	}
	if out, _ := e.svc.PickerListing(ctx, "u1", ""); out.SuggestDiscovery {
		t.Error("dismissed")
	}
	// Another discoverable resource is not the migrated one.
	e.mustConn(t, azmUser, "new", azmEntraConfig("s"))
	if out, _ := e.svc.PickerListing(ctx, "u1", ""); out.SuggestDiscovery {
		t.Error("only the migrated resource suggests discovery in the picker")
	}
}

// R4: a background listing refresh reads its resource's credential when it
// RUNS, not when it is queued: a job still waiting when the credential is
// rotated, deleted or disabled is skipped — it never signs in with the old
// document (which would bring back the Entra ID token source the change
// just released, and list the project as a principal that was removed).
func TestAzureModels_RefreshReadsTheCredentialWhenItRuns(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	for i := 0; i < azureRefreshParallel; i++ {
		c := e.mustConn(t, azmUser, "busy"+strings.Repeat("x", i), azmEntraConfig("busy"))
		e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-4o"})
	}
	rotated := e.mustConn(t, azmUser, "rotated", azmEntraConfig("old-secret"))
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: rotated.ID, Kind: "chat", Deployment: "gpt-4o"})
	deleted := e.mustConn(t, azmUser, "deleted", azmEntraConfig("deleted-secret"))
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: deleted.ID, Kind: "chat", Deployment: "gpt-4o"})
	kept := e.mustConn(t, azmUser, "kept", azmEntraConfig("kept-secret"))
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: kept.ID, Kind: "chat", Deployment: "gpt-4o"})

	release := make(chan struct{})
	var mu sync.Mutex
	var refreshed []string
	busy := 0
	e.svc.refresher = newAzureListingRefresher(func(_ context.Context, raw string, _ []ai.AzureRegisteredModel) error {
		mu.Lock()
		refreshed = append(refreshed, raw)
		isBusy := strings.Contains(raw, `"clientSecret":"busy"`)
		if isBusy {
			busy++
		}
		mu.Unlock()
		if isBusy {
			<-release // every worker held: the other jobs wait in the queue
		}
		return nil
	})
	if _, err := e.svc.PickerListing(ctx, "u1", ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := busy
		mu.Unlock()
		if n == azureRefreshParallel || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// While their jobs wait: one credential rotated, one deleted.
	next := azmEntraConfig("new-secret")
	if _, err := e.svc.UpdateConnection(ctx, azmUser, rotated.ID, AzureConnectionPatch{Config: &next}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DeleteConnection(ctx, azmUser, deleted.ID, AzureDeleteOptions{Cascade: true}); err != nil {
		t.Fatal(err)
	}
	close(release)
	e.svc.refresher.wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	sawKept := false
	for _, raw := range refreshed {
		for _, stale := range []string{"old-secret", "deleted-secret", "new-secret"} {
			if strings.Contains(raw, stale) {
				t.Errorf("a queued refresh ran on a credential it was not queued for (%s): %s", stale, raw)
			}
		}
		sawKept = sawKept || strings.Contains(raw, "kept-secret")
	}
	if !sawKept {
		t.Errorf("the unchanged resource was not refreshed: %d refreshes", len(refreshed))
	}
}
