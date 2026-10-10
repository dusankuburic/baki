package contract

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// contractRuleSet is a rule set as the analyzer records it (refs sorted, a digest); the stores keep it as given, so
// the digest is only an identifier here.
func contractRuleSet(runID, name string, ids ...string) *models.RuleSet {
	rs := &models.RuleSet{Rules: []models.RuleRef{}, Digest: "rs1:contract-" + name + "-" + runID}
	for _, id := range ids {
		rs.Rules = append(rs.Rules, models.RuleRef{ID: id})
	}
	return rs
}

func sameRuleSet(a, b *models.RuleSet) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Digest == b.Digest && slices.Equal(a.Rules, b.Rules)
}

// runRuleSetScenarios holds the rule sets baselines and health snapshots record and the "new rules" notices
// (migration 0037, ADR 0016).
func runRuleSetScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	saveFlow := func(t *testing.T, id, owner string) {
		t.Helper()
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: id, Name: id, Content: json.RawMessage(`{}`), OwnerID: owner}); err != nil {
			t.Fatalf("SaveFlow %s: %v", id, err)
		}
		t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), id) })
	}

	run("FlowBaseline_records_its_rule_set_and_batches_read_the_digest", func(t *testing.T) {
		flowID := "contract-rs-baseline-" + runID
		saveFlow(t, flowID, "contract-rs-owner-"+runID)
		rs := contractRuleSet(runID, "baseline", "r1", "r2")
		if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: flowID, Keys: []string{"r1:x"}, RuleSet: rs}); err != nil {
			t.Fatalf("SetFlowBaseline: %v", err)
		}
		bl, err := b.GetFlowBaseline(ctx, flowID)
		if err != nil || bl == nil || !sameRuleSet(bl.RuleSet, rs) || bl.RuleSetDigest != rs.Digest {
			t.Fatalf("GetFlowBaseline = %+v, %v; want the rule set and its digest", bl, err)
		}
		batch, err := b.GetFlowBaselinesBatch(ctx, []string{flowID})
		if err != nil || batch[flowID] == nil || batch[flowID].RuleSetDigest != rs.Digest || batch[flowID].RuleSet != nil {
			t.Fatalf("GetFlowBaselinesBatch = %+v, %v; want the digest without the rule set", batch[flowID], err)
		}

		// A baseline set without a rule set records none: it reads as a legacy record.
		if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: flowID, Keys: []string{"r1:x"}}); err != nil {
			t.Fatalf("SetFlowBaseline (no rule set): %v", err)
		}
		if bl, err := b.GetFlowBaseline(ctx, flowID); err != nil || bl == nil || bl.RuleSet != nil || bl.RuleSetDigest != "" {
			t.Fatalf("GetFlowBaseline after a set without a rule set = %+v, %v; want none", bl, err)
		}
		if batch, err := b.GetFlowBaselinesBatch(ctx, []string{flowID}); err != nil || batch[flowID] == nil || batch[flowID].RuleSetDigest != "" {
			t.Fatalf("GetFlowBaselinesBatch after a set without a rule set = %+v, %v; want no digest", batch[flowID], err)
		}
	})

	run("SaveFlowAnalysis_records_its_rule_set_digest", func(t *testing.T) {
		flowID := "contract-rs-health-" + runID
		saveFlow(t, flowID, "contract-rs-owner-"+runID)
		rs := contractRuleSet(runID, "health", "r1")
		at := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
		if err := b.SaveFlowAnalysis(ctx, &interfaces.FlowAnalysis{FlowID: flowID, HealthScore: 90, AnalyzedAt: at, RuleSet: rs}); err != nil {
			t.Fatalf("SaveFlowAnalysis: %v", err)
		}
		if h, err := b.LoadFlowHealth(ctx, flowID); err != nil || h == nil || h.RuleSetDigest != rs.Digest {
			t.Fatalf("LoadFlowHealth = %+v, %v; want digest %s", h, err, rs.Digest)
		}
		if m, err := b.LoadFlowHealthBatch(ctx, []string{flowID}); err != nil || m[flowID] == nil || m[flowID].RuleSetDigest != rs.Digest {
			t.Fatalf("LoadFlowHealthBatch = %+v, %v; want digest %s", m[flowID], err, rs.Digest)
		}
		if err := b.SaveFlowAnalysis(ctx, &interfaces.FlowAnalysis{FlowID: flowID, HealthScore: 80, AnalyzedAt: at.Add(time.Second)}); err != nil {
			t.Fatalf("SaveFlowAnalysis (no rule set): %v", err)
		}
		if h, err := b.LoadFlowHealth(ctx, flowID); err != nil || h == nil || h.RuleSetDigest != "" || h.HealthScore != 80 {
			t.Fatalf("LoadFlowHealth after an analysis without a rule set = %+v, %v; want no digest", h, err)
		}
	})

	run("RuleSetsByDigest_resolves_what_baselines_and_snapshots_record", func(t *testing.T) {
		owner := "contract-rs-owner-" + runID
		fBase, fSnap, fGone := "contract-rs-r1-"+runID, "contract-rs-r2-"+runID, "contract-rs-r3-"+runID
		for _, id := range []string{fBase, fSnap, fGone} {
			saveFlow(t, id, owner)
		}
		onBaseline, onSnapshot, replaced := contractRuleSet(runID, "on-baseline", "a", "b"),
			contractRuleSet(runID, "on-snapshot", "a"), contractRuleSet(runID, "replaced", "c")
		if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: fBase, Keys: []string{}, RuleSet: onBaseline}); err != nil {
			t.Fatal(err)
		}
		if err := b.SaveFlowAnalysis(ctx, &interfaces.FlowAnalysis{FlowID: fSnap, HealthScore: 70, RuleSet: onSnapshot}); err != nil {
			t.Fatal(err)
		}
		// Replaced by a baseline without a rule set: no record carries the digest any more.
		if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: fGone, Keys: []string{}, RuleSet: replaced}); err != nil {
			t.Fatal(err)
		}
		if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: fGone, Keys: []string{}}); err != nil {
			t.Fatal(err)
		}

		got, err := b.RuleSetsByDigest(ctx, []string{onBaseline.Digest, onSnapshot.Digest, replaced.Digest, "rs1:contract-none-" + runID})
		if err != nil {
			t.Fatalf("RuleSetsByDigest: %v", err)
		}
		if len(got) != 2 || !sameRuleSet(got[onBaseline.Digest], onBaseline) || !sameRuleSet(got[onSnapshot.Digest], onSnapshot) {
			t.Fatalf("RuleSetsByDigest = %v; want exactly the baseline's and the snapshot's sets", got)
		}
		if empty, err := b.RuleSetsByDigest(ctx, nil); err != nil || empty == nil || len(empty) != 0 {
			t.Fatalf("RuleSetsByDigest(nil) = %v, %v; want a non-nil empty map", empty, err)
		}
	})

	run("InsertGovernanceAlert_reports_whether_it_inserted", func(t *testing.T) {
		flowID := "contract-rs-alert-" + runID
		saveFlow(t, flowID, "contract-rs-owner-"+runID)
		a := &interfaces.GovernanceAlert{ID: flowID + "|drift|e1w0", FlowID: flowID, Type: "drift", Title: "t", Severity: "error"}
		if ok, err := b.InsertGovernanceAlert(ctx, a); err != nil || !ok {
			t.Fatalf("first InsertGovernanceAlert = %v, %v; want true", ok, err)
		}
		again := *a
		again.Title = "changed"
		if ok, err := b.InsertGovernanceAlert(ctx, &again); err != nil || ok {
			t.Fatalf("second InsertGovernanceAlert = %v, %v; want false", ok, err)
		}
		alerts, err := b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{Limit: 500})
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range alerts {
			if got.ID == a.ID && got.Title != "t" {
				t.Errorf("a repeated insert rewrote the alert: %+v", got)
			}
		}
	})

	run("RuleNotices_insert_once_merge_and_keep_their_stamps", func(t *testing.T) {
		member, solo, outsider := "contract-rn-member-"+runID, "contract-rn-solo-"+runID, "contract-rn-outsider-"+runID
		for _, id := range []string{member, solo, outsider} {
			if err := b.CreateUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "hash"}); err != nil {
				t.Fatalf("CreateUser %s: %v", id, err)
			}
			uid := id
			t.Cleanup(func() { _ = b.DeleteUser(context.Background(), uid) })
		}
		orgID := "contract-rn-org-" + runID
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "Notices", OwnerID: member,
			Members: []interfaces.OrgMember{{UserID: member, Role: auth.RoleAdmin}}}); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), orgID) })

		digest := "rs1:contract-notice-" + runID
		notice := func(scope, scopeID string, flows int, ids ...string) *interfaces.RuleNotice {
			return &interfaces.RuleNotice{ID: interfaces.RuleNoticeID(scope, scopeID, digest), Kind: interfaces.RuleNoticeKindNewRules,
				Scope: scope, ScopeID: scopeID, RuleSetDigest: digest, RuleIDs: ids, FlowCount: flows}
		}
		orgNotice := notice(interfaces.RuleNoticeScopeOrg, orgID, 2, "rule-b", "rule-a")
		userNotice := notice(interfaces.RuleNoticeScopeUser, solo, 1, "rule-a")
		ghost := notice(interfaces.RuleNoticeScopeUser, "contract-rn-nobody-"+runID, 1, "rule-a")
		ghostOrg := notice(interfaces.RuleNoticeScopeOrg, "contract-rn-no-org-"+runID, 1, "rule-a")
		unreadBefore := map[string]int{}
		for _, u := range []string{member, solo, outsider} {
			n, err := b.UnreadRuleNoticeCount(ctx, u)
			if err != nil {
				t.Fatalf("UnreadRuleNoticeCount %s: %v", u, err)
			}
			unreadBefore[u] = n
		}

		inserted, err := b.UpsertRuleNotices(ctx, []*interfaces.RuleNotice{orgNotice, userNotice, ghost, ghostOrg})
		if err != nil {
			t.Fatalf("UpsertRuleNotices: %v", err)
		}
		want := []string{orgNotice.ID, userNotice.ID}
		slices.Sort(want)
		if !slices.Equal(inserted, want) {
			t.Fatalf("inserted = %v, want %v (a notice of a missing user or organisation is skipped)", inserted, want)
		}

		mine := func(t *testing.T, user string) []*interfaces.RuleNotice {
			t.Helper()
			all, err := b.ListRuleNotices(ctx, user, 100)
			if err != nil {
				t.Fatalf("ListRuleNotices %s: %v", user, err)
			}
			if all == nil {
				t.Fatalf("ListRuleNotices %s returned nil", user)
			}
			var out []*interfaces.RuleNotice
			for _, n := range all {
				if n.RuleSetDigest == digest {
					out = append(out, n)
				}
			}
			return out
		}
		if got := mine(t, member); len(got) != 1 || got[0].ID != orgNotice.ID || !slices.Equal(got[0].RuleIDs, []string{"rule-a", "rule-b"}) ||
			got[0].FlowCount != 2 || got[0].Kind != interfaces.RuleNoticeKindNewRules || got[0].CreatedAt.IsZero() {
			t.Fatalf("member's notices = %+v; want the org notice with sorted rule ids", got)
		}
		if got := mine(t, solo); len(got) != 1 || got[0].ID != userNotice.ID {
			t.Fatalf("user's notices = %+v; want their own", got)
		}
		if got := mine(t, outsider); len(got) != 0 {
			t.Fatalf("an outsider sees %+v", got)
		}
		if n, _ := b.UnreadRuleNoticeCount(ctx, member); n != unreadBefore[member]+1 {
			t.Errorf("member's unread notices = %d, want %d", n, unreadBefore[member]+1)
		}
		if n, _ := b.UnreadRuleNoticeCount(ctx, outsider); n != unreadBefore[outsider] {
			t.Errorf("outsider's unread notices = %d, want %d", n, unreadBefore[outsider])
		}

		// The member reads it: the stamp is the org's, and an upsert merges without clearing it.
		if err := b.MarkAllRuleNoticesRead(ctx, member); err != nil {
			t.Fatalf("MarkAllRuleNoticesRead: %v", err)
		}
		again := notice(interfaces.RuleNoticeScopeOrg, orgID, 1, "rule-c", "rule-a")
		if inserted, err := b.UpsertRuleNotices(ctx, []*interfaces.RuleNotice{again}); err != nil || len(inserted) != 0 {
			t.Fatalf("upsert of a known notice = %v, %v; want nothing inserted", inserted, err)
		}
		got := mine(t, member)
		if len(got) != 1 || !slices.Equal(got[0].RuleIDs, []string{"rule-a", "rule-b", "rule-c"}) || got[0].FlowCount != 2 || got[0].ReadAt == nil {
			t.Fatalf("after the merge: %+v; want the union of rule ids, flow count 2, still read", got)
		}
		if n, _ := b.UnreadRuleNoticeCount(ctx, member); n != unreadBefore[member] {
			t.Errorf("member's unread notices after reading = %d, want %d", n, unreadBefore[member])
		}

		// Dismissed by someone who cannot see it: nothing; by the user: gone from the list, and it stays dismissed.
		if err := b.DismissRuleNotice(ctx, outsider, userNotice.ID); err != nil {
			t.Fatalf("DismissRuleNotice (outsider): %v", err)
		}
		if got := mine(t, solo); len(got) != 1 {
			t.Fatalf("an outsider's dismiss hid the notice: %+v", got)
		}
		if err := b.DismissRuleNotice(ctx, solo, userNotice.ID); err != nil {
			t.Fatalf("DismissRuleNotice: %v", err)
		}
		if _, err := b.UpsertRuleNotices(ctx, []*interfaces.RuleNotice{userNotice}); err != nil {
			t.Fatal(err)
		}
		if got := mine(t, solo); len(got) != 0 {
			t.Fatalf("a dismissed notice is listed again after an upsert: %+v", got)
		}

		bad := notice(interfaces.RuleNoticeScopeOrg, orgID, 1, "rule-a")
		bad.ID = "new_rules|org|someone-else|" + digest
		if _, err := b.UpsertRuleNotices(ctx, []*interfaces.RuleNotice{bad}); err == nil {
			t.Errorf("a notice whose id is not its scope's and digest's was accepted")
		}
	})

	run("RuleNotices_go_with_their_organisation_and_user", func(t *testing.T) {
		user, orgID := "contract-rn-gone-"+runID, "contract-rn-gone-org-"+runID
		createUser := func() {
			t.Helper()
			if err := b.CreateUser(ctx, &interfaces.User{ID: user, Email: user + "@example.com", Password: "hash"}); err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
		}
		saveOrg := func() {
			t.Helper()
			if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "Gone", OwnerID: user,
				Members: []interfaces.OrgMember{{UserID: user, Role: auth.RoleAdmin}}}); err != nil {
				t.Fatalf("SaveOrg: %v", err)
			}
		}
		createUser()
		saveOrg()
		t.Cleanup(func() {
			_ = b.DeleteOrg(context.Background(), orgID)
			_ = b.DeleteUser(context.Background(), user)
		})
		digest := "rs1:contract-gone-" + runID
		var notices []*interfaces.RuleNotice
		for _, sc := range [][2]string{{interfaces.RuleNoticeScopeOrg, orgID}, {interfaces.RuleNoticeScopeUser, user}} {
			notices = append(notices, &interfaces.RuleNotice{ID: interfaces.RuleNoticeID(sc[0], sc[1], digest),
				Kind: interfaces.RuleNoticeKindNewRules, Scope: sc[0], ScopeID: sc[1], RuleSetDigest: digest, RuleIDs: []string{"rule-a"}})
		}
		if inserted, err := b.UpsertRuleNotices(ctx, notices); err != nil || len(inserted) != 2 {
			t.Fatalf("UpsertRuleNotices = %v, %v; want both inserted", inserted, err)
		}

		if err := b.DeleteOrg(ctx, orgID); err != nil {
			t.Fatalf("DeleteOrg: %v", err)
		}
		if err := b.DeleteUser(ctx, user); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		// Recreated under the same ids, they start without the old notices: an upsert inserts both again.
		createUser()
		saveOrg()
		if inserted, err := b.UpsertRuleNotices(ctx, notices); err != nil || len(inserted) != 2 {
			t.Fatalf("UpsertRuleNotices after the org and the user went = %v, %v; want both inserted again", inserted, err)
		}
	})
}
