package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"pad-analyzer/internal/service"
)

// orgAllowedProviders reads the org's stored allowed-provider list through the settings route.
func orgAllowedProviders(t *testing.T, rt *Router, orgID, bearer string) []string {
	t.Helper()
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/system/settings/org/"+orgID, bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var got struct {
		AI struct {
			AllowedProviders []string `json:"allowedProviders"`
		} `json:"ai"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return got.AI.AllowedProviders
}

// TestOrgSettings_AllowedProvidersSurvivePartialWrites (Phase 4b review C1): the only UI that writes org settings
// (the Rules panel) posts {analysis:{rules}}; a write that does not mention ai.allowedProviders must keep the
// stored list — an absent field is not an empty one, and an empty list allows every provider. An explicit []
// clears it, and an id the registry does not know is refused (400 PROVIDER_UNKNOWN) without touching the list.
func TestOrgSettings_AllowedProvidersSurvivePartialWrites(t *testing.T) {
	rt := newOrgSettingsRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	mirrorOrg(t, rt, orgID)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings/org/"+orgID, bearer,
		map[string]any{"ai": map[string]any{"allowedProviders": []string{"claude"}}})
	checkStatus(t, rr, http.StatusOK)
	if got := orgAllowedProviders(t, rt, orgID, bearer); len(got) != 1 || got[0] != "claude" {
		t.Fatalf("after setting the list: allowedProviders = %v, want [claude]", got)
	}

	// The Rules panel's shape: no ai section at all.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings/org/"+orgID, bearer,
		map[string]any{"analysis": map[string]any{"rules": map[string]any{
			"dead-code": map[string]any{"enabled": false, "severity": "info"},
		}}})
	checkStatus(t, rr, http.StatusOK)
	if got := orgAllowedProviders(t, rt, orgID, bearer); len(got) != 1 || got[0] != "claude" {
		t.Errorf("a rules-only write wiped the allowlist: allowedProviders = %v, want [claude] (empty = every provider allowed)", got)
	}

	// An ai section that does not name the list keeps it too.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings/org/"+orgID, bearer,
		map[string]any{"ai": map[string]any{"dailyBudget": 5}})
	checkStatus(t, rr, http.StatusOK)
	if got := orgAllowedProviders(t, rt, orgID, bearer); len(got) != 1 || got[0] != "claude" {
		t.Errorf("an ai write without the field wiped the allowlist: allowedProviders = %v, want [claude]", got)
	}

	// An unknown id is refused with a code, and the stored list is untouched.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings/org/"+orgID, bearer,
		map[string]any{"ai": map[string]any{"allowedProviders": []string{"Claude"}}})
	checkStatus(t, rr, http.StatusBadRequest)
	if code := errorCode(t, rr); code != service.ProviderCodeUnknown {
		t.Errorf("unknown provider id in the allowlist: code = %q, want %q", code, service.ProviderCodeUnknown)
	}
	if got := orgAllowedProviders(t, rt, orgID, bearer); len(got) != 1 || got[0] != "claude" {
		t.Errorf("a refused write changed the allowlist: allowedProviders = %v, want [claude]", got)
	}

	// An explicit empty list clears it.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings/org/"+orgID, bearer,
		map[string]any{"ai": map[string]any{"allowedProviders": []string{}}})
	checkStatus(t, rr, http.StatusOK)
	if got := orgAllowedProviders(t, rt, orgID, bearer); len(got) != 0 {
		t.Errorf("an explicit [] did not clear the allowlist: allowedProviders = %v", got)
	}
}
