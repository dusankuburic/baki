package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// TestGovernanceAlerts_NewRulesNotice: a "new rules" notice reaches the bell through the existing routes: listed
// first as a new_rules entry without a flow (its rule ids and scope additive), counted in the badge, dismissed by
// its id; a user outside its organisation never sees it.
func TestGovernanceAlerts_NewRulesNotice(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "alice", "alice@example.com", "member")
	seedUserWithRole(t, rt, "bob", "bob@example.com", "member")
	ctx := context.Background()
	b := rt.security.Backend
	if err := b.SaveOrg(ctx, &storageif.Organisation{ID: "org-1", Name: "One", OwnerID: "alice",
		Members: []storageif.OrgMember{{UserID: "alice"}}}); err != nil {
		t.Fatal(err)
	}
	id := storageif.RuleNoticeID("org", "org-1", "rs1:d")
	if _, err := b.UpsertRuleNotices(ctx, []*storageif.RuleNotice{{ID: id, Kind: storageif.RuleNoticeKindNewRules,
		Scope: "org", ScopeID: "org-1", RuleSetDigest: "rs1:d", RuleIDs: []string{"hardcoded-url"}, FlowCount: 4}}); err != nil {
		t.Fatal(err)
	}
	alice, bob := jwtBearer(t, rt, "alice", "alice@example.com"), jwtBearer(t, rt, "bob", "bob@example.com")

	list := func(bearer string) []map[string]any {
		t.Helper()
		rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/governance/alerts", bearer, nil)
		checkStatus(t, rr, http.StatusOK)
		var out []map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	unread := func(bearer string) int {
		t.Helper()
		rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/governance/alerts/unread-count", bearer, nil)
		checkStatus(t, rr, http.StatusOK)
		var c struct{ Count int }
		decodeJSON(t, rr, &c)
		return c.Count
	}

	got := list(alice)
	if len(got) != 1 {
		t.Fatalf("alice lists %v, want the notice", got)
	}
	e := got[0]
	ids, _ := e["ruleIds"].([]any)
	if e["id"] != id || e["type"] != "new_rules" || e["flowId"] != "" || e["scope"] != "org" || e["orgId"] != "org-1" ||
		len(ids) != 1 || ids[0] != "hardcoded-url" || e["flowCount"] != float64(4) || e["title"] == "" {
		t.Errorf("notice entry = %v", e)
	}
	if n := unread(alice); n != 1 {
		t.Errorf("alice's badge = %d, want 1", n)
	}
	if got := list(bob); len(got) != 0 || unread(bob) != 0 {
		t.Errorf("bob, outside the org, sees %v", got)
	}

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/governance/alerts/dismiss", bob, map[string]string{"id": id})
	checkStatus(t, rr, http.StatusOK)
	if got := list(alice); len(got) != 1 {
		t.Fatalf("bob's dismiss hid alice's notice: %v", got)
	}
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/governance/alerts/dismiss", alice, map[string]string{"id": id})
	checkStatus(t, rr, http.StatusOK)
	if got := list(alice); len(got) != 0 || unread(alice) != 0 {
		t.Errorf("after dismissing: %v", got)
	}
}
