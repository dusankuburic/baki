package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// runLifecycleScenarios holds every backend to the data lifecycle the Postgres registry defines
// (internal/storage/database/lifecycle_*.go): what account erasure deletes and what it anonymises, what deleting an
// organisation takes with it, and what the data-subject export carries; and to the organisation service's store
// beyond the reads, the locked update and the invitations. Fixtures are scoped by runID.
func runLifecycleScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	createUser := func(t *testing.T, name string) string {
		t.Helper()
		id := "contract-lc-" + name + "-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "hash"}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), id) })
		return id
	}
	saveFlow := func(t *testing.T, f *interfaces.FlowDocument) {
		t.Helper()
		if f.Content == nil {
			f.Content = json.RawMessage(`{}`)
		}
		if err := b.SaveFlow(ctx, f); err != nil {
			t.Fatalf("SaveFlow %s: %v", f.ID, err)
		}
		id := f.ID
		t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), id) })
	}
	saveOrg := func(t *testing.T, org *interfaces.Organisation) {
		t.Helper()
		if err := b.SaveOrg(ctx, org); err != nil {
			t.Fatalf("SaveOrg %s: %v", org.ID, err)
		}
		id := org.ID
		t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), id) })
	}
	saveInvite := func(t *testing.T, inv *interfaces.OrgInvite) {
		t.Helper()
		if err := b.SaveOrgInvite(ctx, inv); err != nil {
			t.Fatalf("SaveOrgInvite %s: %v", inv.ID, err)
		}
	}
	memberIDs := func(org *interfaces.Organisation) []string {
		var ids []string
		for _, m := range org.Members {
			ids = append(ids, m.UserID)
		}
		slices.Sort(ids)
		return ids
	}

	run("DeleteUser_erases_owned_content_and_clears_the_user_on_shared_rows", func(t *testing.T) {
		u, v := createUser(t, "erase-u"), createUser(t, "erase-v")
		email, name := u+"@example.com", "Ana "+runID
		if err := b.UpdateUserProfile(ctx, u, name, ""); err != nil {
			t.Fatalf("UpdateUserProfile: %v", err)
		}
		ownedOrg, otherOrg := "contract-lc-erase-org-"+runID, "contract-lc-erase-orgp-"+runID
		saveOrg(t, &interfaces.Organisation{ID: ownedOrg, Name: "Owned", OwnerID: u, Members: []interfaces.OrgMember{
			{UserID: u, Role: auth.RoleAdmin}, {UserID: v, Role: auth.RoleAdmin},
		}})
		saveOrg(t, &interfaces.Organisation{ID: otherOrg, Name: "Other", OwnerID: v, Members: []interfaces.OrgMember{{UserID: v, Role: auth.RoleAdmin}}})
		expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		sent := &interfaces.OrgInvite{ID: "contract-lc-erase-sent-" + runID, OrgID: ownedOrg, Email: "guest-" + runID + "@example.com",
			Role: auth.RoleMember, InvitedBy: u, TokenHash: "contract-lc-erase-sent-hash-" + runID, ExpiresAt: expires}
		received := &interfaces.OrgInvite{ID: "contract-lc-erase-received-" + runID, OrgID: otherOrg, Email: email,
			Role: auth.RoleMember, InvitedBy: v, TokenHash: "contract-lc-erase-received-hash-" + runID, ExpiresAt: expires}
		saveInvite(t, sent)
		saveInvite(t, received)

		// The user's own flow; a teammate's flow shared with the user; one in an organisation the user belongs to; and
		// another tenant's flow the user cannot reach.
		own, shared := "contract-lc-erase-own-"+runID, "contract-lc-erase-shared-"+runID
		orgFlow, foreign := "contract-lc-erase-orgflow-"+runID, "contract-lc-erase-foreign-"+runID
		stranger := "contract-lc-erase-w-" + runID
		saveFlow(t, &interfaces.FlowDocument{ID: own, Name: "Own", OwnerID: u})
		saveFlow(t, &interfaces.FlowDocument{ID: shared, Name: "Shared", OwnerID: v})
		saveFlow(t, &interfaces.FlowDocument{ID: orgFlow, Name: "Org", OwnerID: v, OrganizationID: ownedOrg})
		saveFlow(t, &interfaces.FlowDocument{ID: foreign, Name: "Foreign", OwnerID: stranger})
		if err := b.AddCollaborator(ctx, shared, &interfaces.Collaborator{UserID: u, Permission: "editor"}); err != nil {
			t.Fatalf("AddCollaborator: %v", err)
		}
		// A teammate's flow the user commented on and then lost access to.
		lost := "contract-lc-erase-lost-" + runID
		saveFlow(t, &interfaces.FlowDocument{ID: lost, Name: "Lost", OwnerID: v})
		if err := b.AddCollaborator(ctx, lost, &interfaces.Collaborator{UserID: u, Permission: "editor"}); err != nil {
			t.Fatalf("AddCollaborator lost: %v", err)
		}
		if err := b.AddFindingComment(ctx, &interfaces.FindingComment{FlowID: lost, FindingKey: "k", AuthorID: u, AuthorName: name, Body: "before I left"}); err != nil {
			t.Fatalf("AddFindingComment lost: %v", err)
		}
		if err := b.RemoveCollaborator(ctx, lost, u); err != nil {
			t.Fatalf("RemoveCollaborator lost: %v", err)
		}
		if err := b.SaveConversation(ctx, own, u, []interfaces.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteConversation(context.Background(), own, u) })
		mine := &interfaces.FindingComment{FlowID: shared, FindingKey: "k", AuthorID: u, AuthorName: name, Body: "keep my words"}
		theirs := &interfaces.FindingComment{FlowID: shared, FindingKey: "k", AuthorID: v, AuthorName: "Vic", Body: "reply"}
		for _, c := range []*interfaces.FindingComment{mine, theirs} {
			if err := b.AddFindingComment(ctx, c); err != nil {
				t.Fatalf("AddFindingComment: %v", err)
			}
		}
		for _, st := range []*interfaces.FindingStatus{
			{FlowID: shared, FindingKey: "assigned", RuleID: "r", Status: "in_progress", AssigneeID: u, UpdatedBy: v},
			{FlowID: shared, FindingKey: "triaged", RuleID: "r", Status: "acknowledged", UpdatedBy: u},
		} {
			if err := b.SetFindingStatus(ctx, st); err != nil {
				t.Fatalf("SetFindingStatus: %v", err)
			}
		}
		linkHash := "contract-lc-erase-link-" + runID
		if err := b.CreateShareToken(ctx, &interfaces.ShareToken{FlowID: shared, TokenHash: linkHash, CreatedBy: u}); err != nil {
			t.Fatalf("CreateShareToken: %v", err)
		}
		now := time.Now().UTC()
		alert := func(id, flowID, target, typ, message string) *interfaces.GovernanceAlert {
			return &interfaces.GovernanceAlert{ID: "contract-lc-erase-" + id + "-" + runID, FlowID: flowID, Type: typ,
				Title: typ, Message: message, Severity: "warning", TargetUser: target, CreatedAt: now}
		}
		// Teammates' notifications naming the user exactly are anonymised; anything else naming them is left alone: a
		// message that only contains the name or address, another alert type, and another tenant's alert about
		// someone with the same display name.
		toUser := alert("to-user", shared, u, "comment_added", "New comment from Vic")
		byName := alert("by-name", shared, v, "comment_added", "New comment from "+name)
		byEmail := alert("by-email", shared, v, "finding_assigned", "Assigned to you by "+email)
		viaOrg := alert("via-org", orgFlow, v, "finding_assigned", "Assigned to you by "+name)
		lookalike := alert("lookalike", shared, v, "comment_added", "New comment from X"+name)
		longerEmail := alert("longer-email", shared, v, "finding_assigned", "Assigned to you by x"+email)
		drift := alert("drift", shared, v, "drift", "Drift after "+name)
		namesake := alert("namesake", foreign, stranger, "comment_added", "New comment from "+name)
		lostAccess := alert("lost-access", lost, v, "comment_added", "New comment from "+name)
		for _, a := range []*interfaces.GovernanceAlert{toUser, byName, byEmail, viaOrg, lookalike, longerEmail, drift, namesake, lostAccess} {
			if err := b.RecordGovernanceAlert(ctx, a); err != nil {
				t.Fatalf("RecordGovernanceAlert %s: %v", a.ID, err)
			}
		}
		// Pre-auth audit rows about an address (a failed sign-in): the user's, in any case, loses the address and the
		// user agent and keeps the IP; another address's row is untouched.
		since := time.Now().Add(-time.Minute).UTC()
		aboutUser := &interfaces.AuditEvent{ID: "contract-lc-erase-ev-" + runID, Action: "contract.lc.erase", ResourceType: "email",
			ResourceID: strings.ToUpper(email), IP: "203.0.113.7", UserAgent: "agent", CreatedAt: time.Now().UTC()}
		aboutOther := &interfaces.AuditEvent{ID: "contract-lc-erase-ev-other-" + runID, Action: "contract.lc.erase", ResourceType: "email",
			ResourceID: "x" + email, IP: "203.0.113.8", UserAgent: "agent", CreatedAt: time.Now().UTC()}
		for _, ev := range []*interfaces.AuditEvent{aboutUser, aboutOther} {
			if err := b.SaveAuditEvent(ctx, ev); err != nil {
				t.Fatalf("SaveAuditEvent: %v", err)
			}
		}

		if err := b.DeleteUser(ctx, u); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}

		if _, err := b.LoadUserByID(ctx, u); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the erased user loads: %v", err)
		}
		if _, err := b.LoadFlow(ctx, own); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the erased user's flow loads: %v", err)
		}
		if msgs, err := b.LoadConversation(ctx, own, u); err != nil || len(msgs) != 0 {
			t.Errorf("the chat on the erased user's flow survived: %+v, %v", msgs, err)
		}
		comments, err := b.ListFindingComments(ctx, shared, "k")
		if err != nil || len(comments) != 2 {
			t.Fatalf("comments on the shared flow = %+v, %v; want both kept", comments, err)
		}
		for _, c := range comments {
			switch c.ID {
			case mine.ID:
				if c.AuthorID != "" || c.AuthorName != "" || c.Body != "keep my words" {
					t.Errorf("the erased user's comment = %+v; want its body without an author", c)
				}
			case theirs.ID:
				if c.AuthorID != v || c.AuthorName != "Vic" {
					t.Errorf("another author's comment changed: %+v", c)
				}
			}
		}
		if st, err := b.GetFindingStatus(ctx, shared, "assigned"); err != nil || st == nil || st.AssigneeID != "" || st.UpdatedBy != v {
			t.Errorf("the erased user's assignment = %+v, %v; want no assignee, the updater kept", st, err)
		}
		if st, err := b.GetFindingStatus(ctx, shared, "triaged"); err != nil || st == nil || st.UpdatedBy != "" || st.Status != "acknowledged" {
			t.Errorf("the erased user's triage = %+v, %v; want the decision without its updater", st, err)
		}
		if link, err := b.GetShareTokenByHash(ctx, linkHash); err != nil || link.CreatedBy != "" {
			t.Errorf("the erased user's share link = %+v, %v; want it kept without its creator", link, err)
		}
		if org, err := b.LoadOrg(ctx, ownedOrg); err != nil || org.OwnerID != "" || !slices.Equal(memberIDs(org), []string{v}) {
			t.Errorf("the erased user's organisation = %+v, %v; want it kept with the other admin and no owner", org, err)
		}
		if inv, err := b.GetOrgInvite(ctx, ownedOrg, sent.ID); err != nil || inv.InvitedBy != "" {
			t.Errorf("the erased user's invitation = %+v, %v; want it kept without its inviter", inv, err)
		}
		if _, err := b.GetOrgInvite(ctx, otherOrg, received.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the invitation to the erased user's email: %v, want ErrNotFound", err)
		}
		events, err := b.ListAuditEvents(ctx, interfaces.AuditFilter{Action: "contract.lc.erase", Since: &since, Limit: 500})
		if err != nil {
			t.Fatalf("ListAuditEvents: %v", err)
		}
		seen := 0
		for _, ev := range events {
			switch ev.ID {
			case aboutUser.ID:
				seen++
				if ev.ResourceID != "" || ev.UserAgent != "" || ev.IP != aboutUser.IP {
					t.Errorf("the pre-auth row about the erased address = %+v; want no address or user agent, the IP kept", ev)
				}
			case aboutOther.ID:
				seen++
				if ev.ResourceID != aboutOther.ResourceID || ev.UserAgent != "agent" {
					t.Errorf("another address's row changed: %+v", ev)
				}
			}
		}
		if seen != 2 {
			t.Errorf("found %d of the 2 pre-auth rows", seen)
		}

		want := map[string]string{
			byName.ID:      "New comment from a teammate",
			byEmail.ID:     "Assigned to you by a teammate",
			viaOrg.ID:      "Assigned to you by a teammate",
			lookalike.ID:   lookalike.Message,
			longerEmail.ID: longerEmail.Message,
			drift.ID:       drift.Message,
			namesake.ID:    namesake.Message,
			lostAccess.ID:  "New comment from a teammate",
		}
		for _, reader := range []string{v, stranger} {
			alerts, err := b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{UserID: reader, Limit: 50})
			if err != nil {
				t.Fatalf("ListGovernanceAlerts(%s): %v", reader, err)
			}
			for _, a := range alerts {
				if msg, ok := want[a.ID]; ok {
					if a.Message != msg {
						t.Errorf("alert %s reads %q, want %q", a.ID, a.Message, msg)
					}
					delete(want, a.ID)
				}
			}
		}
		if len(want) > 0 {
			t.Errorf("alerts lost: %v", want)
		}

		// The alert addressed to the erased account is gone: an account that comes back under the same id with
		// access to the flow sees none.
		if err := b.CreateUser(ctx, &interfaces.User{ID: u, Email: "back-" + email, Password: "hash"}); err != nil {
			t.Fatalf("CreateUser (again): %v", err)
		}
		if err := b.AddCollaborator(ctx, shared, &interfaces.Collaborator{UserID: u, Permission: "viewer"}); err != nil {
			t.Fatalf("AddCollaborator: %v", err)
		}
		again, err := b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{UserID: u, Limit: 50})
		if err != nil {
			t.Fatalf("ListGovernanceAlerts (again): %v", err)
		}
		for _, a := range again {
			if a.ID == toUser.ID {
				t.Errorf("the alert addressed to the erased account survived: %+v", a)
			}
		}
	})

	run("DeleteOrg_removes_the_org_configuration_and_detaches_its_flows", func(t *testing.T) {
		admin := createUser(t, "orgdel-admin")
		orgID, otherOrg := "contract-lc-orgdel-"+runID, "contract-lc-orgdel-other-"+runID
		saveOrg(t, &interfaces.Organisation{ID: orgID, Name: "Doomed", OwnerID: admin, Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}})
		flowID := "contract-lc-orgdel-flow-" + runID
		saveFlow(t, &interfaces.FlowDocument{ID: flowID, Name: "Org flow", OwnerID: admin, OrganizationID: orgID})

		policy := &models.Policy{ID: "contract-lc-orgdel-pol-" + runID, OrgID: orgID, Name: "Gate"}
		kept := &models.Policy{ID: "contract-lc-orgdel-pol-" + runID, OrgID: otherOrg, Name: "Kept"}
		for _, p := range []*models.Policy{policy, kept} {
			if err := b.SavePolicy(ctx, p); err != nil {
				t.Fatalf("SavePolicy: %v", err)
			}
		}
		t.Cleanup(func() { _ = b.DeletePolicy(context.Background(), otherOrg, kept.ID) })
		if err := b.SaveOrgChannel(ctx, &interfaces.OrgChannel{ID: "contract-lc-orgdel-ch-" + runID, OrgID: orgID, Name: "Ops",
			Kind: "webhook", URL: "https://example.com/hook", Enabled: true, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("SaveOrgChannel: %v", err)
		}
		if err := b.SaveOrgCustomRule(ctx, &interfaces.OrgCustomRule{ID: "contract-lc-orgdel-rule-" + runID, OrgID: orgID, RuleID: "house-style",
			Config: json.RawMessage(`{"id":"house-style","name":"House style","severity":"warning","rawTypeMatch":"^SET$"}`), Enabled: true}); err != nil {
			t.Fatalf("SaveOrgCustomRule: %v", err)
		}
		docID := "contract-lc-orgdel-doc-" + runID
		if err := b.SaveKnowledgeDocument(ctx, admin, &interfaces.KnowledgeDocument{ID: docID, OrgID: orgID, Filename: "kb.md"}); err != nil {
			t.Fatalf("SaveKnowledgeDocument: %v", err)
		}
		if err := b.SaveKnowledgeChunks(ctx, admin, []interfaces.KnowledgeChunk{
			{ID: docID + "-0", DocID: docID, OrgID: orgID, Content: "chunk", Embedding: []float32{}},
		}); err != nil {
			t.Fatalf("SaveKnowledgeChunks: %v", err)
		}
		invite := &interfaces.OrgInvite{ID: "contract-lc-orgdel-inv-" + runID, OrgID: orgID, Email: "guest-" + runID + "@example.com",
			Role: auth.RoleMember, InvitedBy: admin, TokenHash: "contract-lc-orgdel-inv-hash-" + runID, ExpiresAt: time.Now().Add(time.Hour)}
		saveInvite(t, invite)
		if err := b.SaveOrgSettings(ctx, orgID, &interfaces.AppSettings{Version: 1, Appearance: interfaces.AppearanceSettings{Density: "compact"}}); err != nil {
			t.Fatalf("SaveOrgSettings: %v", err)
		}

		if err := b.DeleteOrg(ctx, orgID); err != nil {
			t.Fatalf("DeleteOrg: %v", err)
		}

		if _, err := b.LoadOrg(ctx, orgID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("LoadOrg after delete: %v, want ErrNotFound", err)
		}
		if f, err := b.LoadFlow(ctx, flowID); err != nil || f.OrganizationID != "" || f.OwnerID != admin {
			t.Errorf("the org's flow = %+v, %v; want it kept by its owner without the org", f, err)
		}
		if ps, err := b.ListPolicies(ctx, orgID); err != nil || len(ps) != 0 {
			t.Errorf("the org's policies survived: %+v, %v", ps, err)
		}
		if p, err := b.GetPolicy(ctx, otherOrg, kept.ID); err != nil || p.Name != "Kept" {
			t.Errorf("another org's policy = %+v, %v; want it kept", p, err)
		}
		if chs, err := b.ListOrgChannels(ctx, orgID, false); err != nil || len(chs) != 0 {
			t.Errorf("the org's channels survived: %+v, %v", chs, err)
		}
		if rules, err := b.ListOrgCustomRules(ctx, orgID, false); err != nil || len(rules) != 0 {
			t.Errorf("the org's custom rules survived: %+v, %v", rules, err)
		}
		if docs, err := b.ListKnowledgeDocuments(ctx, orgID, 10, 0); err != nil || len(docs) != 0 {
			t.Errorf("the org's knowledge documents survived: %+v, %v", docs, err)
		}
		if total, _, err := b.CountKnowledgeChunks(ctx, orgID); err != nil || total != 0 {
			t.Errorf("the org's knowledge chunks survived: %d, %v", total, err)
		}
		if _, err := b.GetOrgInvite(ctx, orgID, invite.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the org's invitation: %v, want ErrNotFound", err)
		}
		if _, err := b.GetOrgInviteByTokenHash(ctx, invite.TokenHash); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the org's invitation by token: %v, want ErrNotFound", err)
		}
		if s, err := b.LoadOrgSettings(ctx, orgID); err != nil || s.Appearance.Density != "" {
			t.Errorf("the org's settings survived: %+v, %v", s, err)
		}
		if err := b.DeleteOrg(ctx, orgID); err != nil {
			t.Errorf("DeleteOrg (missing): %v, want nil", err)
		}
	})

	run("ExportUserData_carries_owned_flows_own_membership_and_never_null_lists", func(t *testing.T) {
		u, v := createUser(t, "export-u"), createUser(t, "export-v")
		own, theirs := "contract-lc-export-own-"+runID, "contract-lc-export-theirs-"+runID
		saveFlow(t, &interfaces.FlowDocument{ID: own, Name: "Own", OwnerID: u})
		saveFlow(t, &interfaces.FlowDocument{ID: theirs, Name: "Theirs", OwnerID: v})
		if err := b.AddCollaborator(ctx, theirs, &interfaces.Collaborator{UserID: u, Permission: "editor"}); err != nil {
			t.Fatalf("AddCollaborator: %v", err)
		}
		for _, flowID := range []string{own, theirs} {
			if err := b.SaveConversation(ctx, flowID, "s1", []interfaces.ChatMessage{{Role: "user", Content: "about " + flowID}}); err != nil {
				t.Fatalf("SaveConversation: %v", err)
			}
			id := flowID
			t.Cleanup(func() { _ = b.DeleteConversation(context.Background(), id, "s1") })
		}
		for _, st := range []*interfaces.FindingStatus{
			{FlowID: own, FindingKey: "k1", RuleID: "r", Status: "acknowledged", UpdatedBy: u},
			{FlowID: own, FindingKey: "k2", RuleID: "r", Status: "acknowledged", UpdatedBy: v},
			{FlowID: theirs, FindingKey: "k3", RuleID: "r", Status: "acknowledged", UpdatedBy: u},
		} {
			if err := b.SetFindingStatus(ctx, st); err != nil {
				t.Fatalf("SetFindingStatus: %v", err)
			}
		}
		authored := &interfaces.FindingComment{FlowID: theirs, FindingKey: "k3", AuthorID: u, Body: "mine"}
		for _, c := range []*interfaces.FindingComment{authored, {FlowID: own, FindingKey: "k1", AuthorID: v, Body: "theirs"}} {
			if err := b.AddFindingComment(ctx, c); err != nil {
				t.Fatalf("AddFindingComment: %v", err)
			}
		}
		orgID := "contract-lc-export-org-" + runID
		saveOrg(t, &interfaces.Organisation{ID: orgID, Name: "Team", OwnerID: v, Members: []interfaces.OrgMember{
			{UserID: v, Role: auth.RoleAdmin}, {UserID: u, Role: auth.RoleMember},
		}})

		export, err := b.ExportUserData(ctx, u)
		if err != nil {
			t.Fatalf("ExportUserData: %v", err)
		}
		if got := flowIDs(export.Flows); !slices.Equal(got, []string{own}) {
			t.Errorf("exported flows = %v, want the owned one only", got)
		}
		if len(export.Conversations) != 1 || export.Conversations[0].FlowID != own || export.Conversations[0].Scope != "s1" {
			t.Errorf("exported conversations = %+v, want the chat on the owned flow only", export.Conversations)
		}
		if len(export.TriageDecisions) != 1 || export.TriageDecisions[0].FlowID != own || export.TriageDecisions[0].FindingKey != "k1" {
			t.Errorf("exported triage = %+v, want the user's decision on the owned flow only", export.TriageDecisions)
		}
		if len(export.Comments) != 1 || export.Comments[0].ID != authored.ID {
			t.Errorf("exported comments = %+v, want the one the user wrote (on another's flow)", export.Comments)
		}
		if len(export.Organisations) != 1 || export.Organisations[0].ID != orgID || !slices.Equal(memberIDs(export.Organisations[0]), []string{u}) {
			t.Errorf("exported organisations = %+v, want the team with the user's own membership only", export.Organisations)
		}

		// Both paths write the same bundle.
		var streamed bytes.Buffer
		if err := b.ExportUserDataTo(ctx, u, &streamed); err != nil {
			t.Fatalf("ExportUserDataTo: %v", err)
		}
		buffered, err := json.Marshal(export)
		if err != nil {
			t.Fatal(err)
		}
		if a, s := withoutExportedAt(t, buffered), withoutExportedAt(t, streamed.Bytes()); !bytes.Equal(a, s) {
			t.Errorf("the streamed bundle differs from the buffered one:\n buffered %s\n streamed %s", a, s)
		}

		// A user with nothing gets empty lists, not nulls.
		empty := createUser(t, "export-empty")
		var none bytes.Buffer
		if err := b.ExportUserDataTo(ctx, empty, &none); err != nil {
			t.Fatalf("ExportUserDataTo (empty): %v", err)
		}
		var sections map[string]json.RawMessage
		if err := json.Unmarshal(none.Bytes(), &sections); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"flows", "auditEvents", "apiTokens", "conversations", "comments", "triageDecisions", "organisations", "sessions"} {
			if string(sections[key]) != "[]" {
				t.Errorf("%s = %s, want []", key, sections[key])
			}
		}
		if _, err := b.ExportUserData(ctx, "contract-lc-export-nobody-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("unknown user: %v, want ErrNotFound", err)
		}
	})

	run("OrgInvites_insert_once_list_newest_first_accept_once_delete", func(t *testing.T) {
		admin := "contract-lc-inv-admin-" + runID
		orgID, otherOrg := "contract-lc-inv-org-"+runID, "contract-lc-inv-other-"+runID
		saveOrg(t, &interfaces.Organisation{ID: orgID, Name: "Inviting", OwnerID: admin, Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}})
		saveOrg(t, &interfaces.Organisation{ID: otherOrg, Name: "Other", OwnerID: admin, Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}})
		base := time.Now().UTC().Truncate(time.Second)
		invite := func(name string, created time.Time) *interfaces.OrgInvite {
			return &interfaces.OrgInvite{ID: "contract-lc-inv-" + name + "-" + runID, OrgID: orgID, Email: name + "-" + runID + "@example.com",
				Role: auth.RoleMember, InvitedBy: admin, TokenHash: "contract-lc-inv-hash-" + name + "-" + runID,
				ExpiresAt: base.Add(time.Hour), CreatedAt: created}
		}
		first, second, stamped := invite("first", base.Add(-2*time.Minute)), invite("second", base.Add(-time.Minute)), invite("stamped", time.Time{})
		for _, inv := range []*interfaces.OrgInvite{first, second, stamped} {
			saveInvite(t, inv)
		}
		if stamped.CreatedAt.IsZero() {
			t.Error("SaveOrgInvite did not stamp a zero CreatedAt")
		}

		again := invite("first", base)
		again.ID = "contract-lc-inv-again-" + runID
		if err := b.SaveOrgInvite(ctx, again); !errors.Is(err, interfaces.ErrOrgInviteExists) {
			t.Errorf("a second pending invite for the email: %v, want ErrOrgInviteExists", err)
		}
		sameID := invite("other-email", base)
		sameID.ID = first.ID
		if err := b.SaveOrgInvite(ctx, sameID); !errors.Is(err, interfaces.ErrOrgInviteExists) {
			t.Errorf("an invite with a taken id: %v, want ErrOrgInviteExists", err)
		}
		orphan := invite("orphan", base)
		orphan.OrgID = "contract-lc-inv-no-org-" + runID
		if err := b.SaveOrgInvite(ctx, orphan); err == nil || errors.Is(err, interfaces.ErrOrgInviteExists) {
			t.Errorf("an invite to a missing organisation: %v, want the foreign key's error", err)
		}

		list, err := b.ListOrgInvites(ctx, orgID)
		if err != nil {
			t.Fatalf("ListOrgInvites: %v", err)
		}
		var ids []string
		for _, inv := range list {
			ids = append(ids, inv.ID)
		}
		if !slices.Equal(ids, []string{stamped.ID, second.ID, first.ID}) {
			t.Errorf("ListOrgInvites = %v, want newest first", ids)
		}
		got, err := b.GetOrgInvite(ctx, orgID, first.ID)
		if err != nil || got.Email != first.Email || got.Role != auth.RoleMember || got.InvitedBy != admin ||
			got.TokenHash != first.TokenHash || !got.ExpiresAt.Equal(first.ExpiresAt) || !got.CreatedAt.Equal(first.CreatedAt) || got.AcceptedAt != nil {
			t.Errorf("GetOrgInvite = %+v, %v; want the stored invite", got, err)
		}
		if _, err := b.GetOrgInvite(ctx, otherOrg, first.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("another org's invite: %v, want ErrNotFound", err)
		}
		if got, err := b.GetOrgInviteByTokenHash(ctx, second.TokenHash); err != nil || got.ID != second.ID {
			t.Errorf("GetOrgInviteByTokenHash = %+v, %v", got, err)
		}
		if _, err := b.GetOrgInviteByTokenHash(ctx, "contract-lc-inv-no-hash-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("an unknown token: %v, want ErrNotFound", err)
		}

		accepted := base.Add(-30 * time.Second)
		if err := b.MarkOrgInviteAccepted(ctx, first.ID, accepted); err != nil {
			t.Fatalf("MarkOrgInviteAccepted: %v", err)
		}
		if err := b.MarkOrgInviteAccepted(ctx, first.ID, base); !errors.Is(err, interfaces.ErrOrgInviteAlreadyAccepted) {
			t.Errorf("a second acceptance: %v, want ErrOrgInviteAlreadyAccepted", err)
		}
		if err := b.MarkOrgInviteAccepted(ctx, "contract-lc-inv-none-"+runID, base); !errors.Is(err, interfaces.ErrOrgInviteAlreadyAccepted) {
			t.Errorf("accepting a missing invite: %v, want ErrOrgInviteAlreadyAccepted", err)
		}
		if got, err := b.GetOrgInviteByTokenHash(ctx, first.TokenHash); err != nil || got.AcceptedAt == nil || !got.AcceptedAt.Equal(accepted) {
			t.Errorf("the accepted invite = %+v, %v; want it stamped %v", got, err, accepted)
		}
		// Only a pending invite holds the email: after the acceptance a new one may be sent.
		if err := b.SaveOrgInvite(ctx, again); err != nil {
			t.Errorf("a new invite after the acceptance: %v", err)
		}

		if err := b.DeleteOrgInvite(ctx, otherOrg, second.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("deleting through another org: %v, want ErrNotFound", err)
		}
		if err := b.DeleteOrgInvite(ctx, orgID, second.ID); err != nil {
			t.Errorf("DeleteOrgInvite: %v", err)
		}
		if err := b.DeleteOrgInvite(ctx, orgID, second.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("deleting it again: %v, want ErrNotFound", err)
		}
		if list, err := b.ListOrgInvites(ctx, otherOrg); err != nil || len(list) != 0 {
			t.Errorf("an org without invites lists %+v, %v", list, err)
		}
	})

	run("MutateOrg_saves_the_change_or_nothing", func(t *testing.T) {
		owner, member, joiner := "contract-lc-mut-owner-"+runID, "contract-lc-mut-member-"+runID, "contract-lc-mut-joiner-"+runID
		orgID := "contract-lc-mut-org-" + runID
		ownerJoined := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
		memberJoined := ownerJoined.Add(30 * time.Minute)
		saveOrg(t, &interfaces.Organisation{ID: orgID, Name: "Before", OwnerID: owner, Members: []interfaces.OrgMember{
			{UserID: owner, Role: auth.RoleAdmin, JoinedAt: ownerJoined}, {UserID: member, Role: auth.RoleMember, JoinedAt: memberJoined},
		}})
		before, err := b.LoadOrg(ctx, orgID)
		if err != nil {
			t.Fatalf("LoadOrg: %v", err)
		}

		if err := b.MutateOrg(ctx, "contract-lc-mut-none-"+runID, func(*interfaces.Organisation) error { return nil }); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("MutateOrg of a missing org: %v, want ErrNotFound", err)
		}
		refused := errors.New("refused")
		if err := b.MutateOrg(ctx, orgID, func(o *interfaces.Organisation) error {
			o.Name, o.Members = "Refused", o.Members[:1]
			return refused
		}); !errors.Is(err, refused) {
			t.Errorf("MutateOrg = %v, want fn's error", err)
		}
		if got, err := b.LoadOrg(ctx, orgID); err != nil || got.Name != "Before" || len(got.Members) != 2 {
			t.Errorf("after a refused mutation: %+v, %v; want nothing written", got, err)
		}

		updated := ownerJoined.Add(45 * time.Minute)
		if err := b.MutateOrg(ctx, orgID, func(o *interfaces.Organisation) error {
			o.Name, o.OwnerID, o.UpdatedAt = "After", member, updated
			o.CreatedAt = updated
			for i := range o.Members {
				o.Members[i].JoinedAt = updated
				if o.Members[i].UserID == member {
					o.Members[i].Role = auth.RoleAdmin
				}
			}
			o.Members = append(o.Members, interfaces.OrgMember{UserID: joiner, Role: auth.RoleViewer})
			return nil
		}); err != nil {
			t.Fatalf("MutateOrg: %v", err)
		}
		after, err := b.LoadOrg(ctx, orgID)
		if err != nil {
			t.Fatalf("LoadOrg: %v", err)
		}
		if after.Name != "After" || after.OwnerID != member || !after.UpdatedAt.Equal(updated) || !after.CreatedAt.Equal(before.CreatedAt) {
			t.Errorf("after the mutation: name %q, owner %q, updated %v, created %v; want After, %s, %v and the creation time kept",
				after.Name, after.OwnerID, after.UpdatedAt, after.CreatedAt, member, updated)
		}
		roster := map[string]interfaces.OrgMember{}
		for _, m := range after.Members {
			roster[m.UserID] = m
		}
		if m := roster[owner]; len(roster) != 3 || m.Role != auth.RoleAdmin || !m.JoinedAt.Equal(ownerJoined) {
			t.Errorf("the owner's membership = %+v (of %d); want admin since %v", m, len(roster), ownerJoined)
		}
		if m := roster[member]; m.Role != auth.RoleAdmin || !m.JoinedAt.Equal(memberJoined) {
			t.Errorf("the promoted member = %+v; want admin since %v", m, memberJoined)
		}
		if m := roster[joiner]; m.Role != auth.RoleViewer || m.JoinedAt.IsZero() {
			t.Errorf("the new member = %+v; want a viewer with a joining time", m)
		}

		if err := b.MutateOrg(ctx, orgID, func(o *interfaces.Organisation) error {
			o.Name = "Twice"
			o.Members = append(o.Members, interfaces.OrgMember{UserID: owner, Role: auth.RoleAdmin})
			return nil
		}); err == nil {
			t.Error("MutateOrg listing a member twice succeeded")
		}
		if err := b.MutateOrg(ctx, orgID, func(o *interfaces.Organisation) error {
			o.Members = slices.DeleteFunc(o.Members, func(m interfaces.OrgMember) bool { return m.UserID == joiner })
			return nil
		}); err != nil {
			t.Fatalf("MutateOrg (remove): %v", err)
		}
		if got, err := b.LoadOrg(ctx, orgID); err != nil || got.Name != "After" || !slices.Equal(memberIDs(got), []string{member, owner}) {
			t.Errorf("after the removal: %+v, %v; want the name kept and the joiner gone", got, err)
		}
	})
}

// withoutExportedAt returns the bundle with its exportedAt value blanked, the one field the two export paths stamp
// apart.
func withoutExportedAt(t *testing.T, bundle []byte) []byte {
	t.Helper()
	const key = `,"exportedAt":"`
	i := bytes.LastIndex(bundle, []byte(key))
	if i < 0 {
		t.Fatalf("no exportedAt in %s", bundle)
	}
	end := bytes.IndexByte(bundle[i+len(key):], '"')
	if end < 0 {
		t.Fatalf("unterminated exportedAt in %s", bundle)
	}
	return slices.Concat(bundle[:i+len(key)], bundle[i+len(key)+end:])
}
