package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// lifecycleFixture seeds rows through the backend or, where that is simpler, plain SQL.
type lifecycleFixture struct {
	t   *testing.T
	b   *PostgresStorageBackend
	ctx context.Context
}

func (f lifecycleFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.b.db.ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatalf("%s: %v", strings.Join(strings.Fields(query), " "), err)
	}
}

func (f lifecycleFixture) ok(what string, err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("%s: %v", what, err)
	}
}

func (f lifecycleFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.b.db.QueryRowContext(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (f lifecycleFixture) str(query string, args ...any) string {
	f.t.Helper()
	var s string
	if err := f.b.db.QueryRowContext(f.ctx, query, args...).Scan(&s); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return s
}

// identityHit is a row holding one of the needles in one column.
type identityHit struct {
	table, column string
	row           map[string]any
}

// scanForIdentity searches every text, uuid and JSON column of every table for any of the needles.
func (f lifecycleFixture) scanForIdentity(needles ...string) []identityHit {
	f.t.Helper()
	rows, err := f.b.db.QueryContext(f.ctx, `
		SELECT c.table_name, c.column_name FROM information_schema.columns c
		  JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		 WHERE c.table_schema = current_schema() AND t.table_type = 'BASE TABLE'
		   AND c.data_type IN ('text', 'character varying', 'character', 'uuid', 'json', 'jsonb')
		 ORDER BY 1, 2`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	type col struct{ table, column string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.column); err != nil {
			f.t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	ident := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	var hits []identityHit
	for _, c := range cols {
		for _, needle := range needles {
			q := fmt.Sprintf(`SELECT to_jsonb(t)::text FROM %s t WHERE strpos(t.%s::text, $1) > 0`, ident(c.table), ident(c.column))
			found, err := rowsOfStrings(f.ctx, f.b.db, q, needle)
			if err != nil {
				f.t.Fatalf("scan %s.%s: %v", c.table, c.column, err)
			}
			for _, raw := range found {
				var row map[string]any
				if err := json.Unmarshal([]byte(raw), &row); err != nil {
					f.t.Fatal(err)
				}
				hits = append(hits, identityHit{table: c.table, column: c.column, row: row})
			}
		}
	}
	return hits
}

// erasureSurvivor is a place the erased user's id or email may remain, and why.
type erasureSurvivor struct {
	table, column, why string
	match              func(row map[string]any) bool
}

// TestPostgres_Lifecycle_ErasureSweep seeds one user with rows in every table that can reference a user (own flows,
// other people's flows they collaborated on, commented on, were assigned on or created links on, a flow they can no
// longer open, organisations, invitations, Azure, tokens, settings, conversations, the audit trail), erases the
// account inside the user's own row-level-security scope as the account route does, then searches every text, uuid
// and JSON column of every table for the user's id and email. Only the listed survivors may remain.
func TestPostgres_Lifecycle_ErasureSweep(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	f := lifecycleFixture{t: t, b: b, ctx: ctx}
	run := fmt.Sprintf("%x", time.Now().UnixNano())

	u, v, w := "erase-u-"+run, "erase-v-"+run, "erase-w-"+run
	email := "erase-u-" + run + "@example.com"
	const name = "Ana Sweep"
	f.ok("SaveUser u", b.SaveUser(ctx, &interfaces.User{ID: u, Email: email, Password: "x", DisplayName: name}))
	f.ok("SaveUser v", b.SaveUser(ctx, &interfaces.User{ID: v, Email: "v-" + run + "@example.com", Password: "x", DisplayName: "Vic Other"}))
	f.ok("SaveUser w", b.SaveUser(ctx, &interfaces.User{ID: w, Email: "w-" + run + "@example.com", Password: "x"}))

	orgO, orgP := "erase-org-o-"+run, "erase-org-p-"+run
	f.ok("SaveOrg O", b.SaveOrg(ctx, &interfaces.Organisation{ID: orgO, Name: "O", OwnerID: u,
		Members: []interfaces.OrgMember{{UserID: u, Role: auth.RoleAdmin}, {UserID: w, Role: auth.RoleMember}}}))
	f.ok("SaveOrg P", b.SaveOrg(ctx, &interfaces.Organisation{ID: orgP, Name: "P", OwnerID: v,
		Members: []interfaces.OrgMember{{UserID: v, Role: auth.RoleAdmin}, {UserID: u, Role: auth.RoleMember}}}))

	own, shared, orgFlow, lost := "erase-own-"+run, "erase-shared-"+run, "erase-orgflow-"+run, "erase-lost-"+run
	stranger := "erase-stranger-" + run
	for _, fl := range []*interfaces.FlowDocument{
		{ID: own, Name: "own", OwnerID: u, OrganizationID: orgO},
		{ID: shared, Name: "shared", OwnerID: v},
		{ID: orgFlow, Name: "org flow", OwnerID: v, OrganizationID: orgP},
		// A flow the user worked on and can no longer open: their rows on it must still be erased.
		{ID: lost, Name: "lost", OwnerID: v},
		// Another tenant's flow the user never had access to.
		{ID: stranger, Name: "stranger", OwnerID: w},
	} {
		fl.Content = json.RawMessage(`{}`)
		f.ok("SaveFlow "+fl.ID, b.SaveFlow(ctx, fl))
	}
	f.ok("AddCollaborator", b.AddCollaborator(ctx, shared, &interfaces.Collaborator{UserID: u, Permission: "edit"}))

	// Account rows.
	f.ok("SaveUserSettings", b.SaveUserSettings(ctx, u, &interfaces.AppSettings{Version: 1}))
	f.ok("StoreRefreshToken", b.StoreRefreshToken(ctx, "jti-"+run, u, time.Now().Add(time.Hour), "agent", "203.0.113.1"))
	f.ok("SaveIdentityLink", b.SaveIdentityLink(ctx, &interfaces.IdentityLink{Provider: "entra", Subject: "sub-" + run, UserID: u, Email: email}))
	f.ok("CreateAPIToken", b.CreateAPIToken(ctx, &interfaces.APIToken{ID: "tok-" + run, UserID: u, Name: "ci", TokenHash: "h-" + run}))
	f.ok("CreateUserToken", b.CreateUserToken(ctx, &interfaces.UserToken{TokenHash: "ut-" + run, Purpose: "reset", UserID: u, ExpiresAt: time.Now().Add(time.Hour)}))
	f.exec(`INSERT INTO idempotency_keys (user_id, idem_key, fingerprint, expires_at) VALUES ($1, 'idem', 'fp', NOW() + INTERVAL '1 day')`, u)
	f.exec(`INSERT INTO provider_keys (user_id, provider, ciphertext) VALUES ($1, 'openai', 'sealed')`, u)
	f.ok("SaveUsageMetric", b.SaveUsageMetric(ctx, &interfaces.UsageMetric{ID: "use-" + run, UserID: u, OrgID: orgO, Provider: "openai", Model: "m"}))
	reg := NewPostgresAzureRegistry(b)
	_, err := reg.CreateConnection(ctx, interfaces.AzureUserOwner(u), interfaces.AzureConnectionRecord{ID: "default", Name: "mine"})
	f.ok("CreateConnection", err)
	_, err = reg.CreateModel(ctx, interfaces.AzureUserOwner(u), interfaces.AzureModelRecord{ID: "docswriter", ConnectionID: "default",
		Kind: interfaces.AzureKindClaude, Deployment: "claude-sonnet-4-6", DisplayName: "Docs writer", Purpose: interfaces.AzurePurposeCustom,
		PurposeCustom: "Docs", ContextWindow: 200_000, MaxOutputTokens: 8192})
	f.ok("CreateModel", err)
	f.exec(`INSERT INTO user_azure_registry_state (user_id) VALUES ($1)`, u)

	// The audit trail: the user's own action, a pre-auth row about the user, and two of another actor's actions
	// naming the user.
	for _, ev := range []*interfaces.AuditEvent{
		{ID: "ev-own-" + run, UserID: u, Email: email, Action: "flow.save", ResourceType: "flow", ResourceID: own, IP: "203.0.113.2", TokenID: "tok-" + run, UserAgent: "agent"},
		{ID: "ev-pre-" + run, Action: "auth.login", ResourceType: "user", ResourceID: u, IP: "203.0.113.3", UserAgent: "agent"},
		{ID: "ev-pre-email-" + run, Action: "auth.login_failure", ResourceType: "email", ResourceID: email, IP: "203.0.113.4", UserAgent: "agent"},
		{ID: "ev-share-" + run, UserID: v, Action: "flow.share", ResourceType: "flow", ResourceID: shared, Meta: map[string]string{"targetUser": u}},
		{ID: "ev-invite-" + run, UserID: v, Action: "org.invite.create", ResourceType: "org", ResourceID: orgP, Meta: map[string]string{"email": email}},
	} {
		ev.CreatedAt = time.Now().UTC()
		f.ok("SaveAuditEvent", b.SaveAuditEvent(ctx, ev))
	}

	// Flow rows: on the user's own flow (they go with it) and on other people's (the identity goes, the content stays).
	f.ok("SaveConversation", b.SaveConversation(ctx, own, "flow", []interfaces.ChatMessage{{ID: "m", Role: "user", Content: "hello"}}))
	for _, fv := range []*interfaces.FlowVersion{
		{ID: "ver-own-" + run, FlowID: own, CreatedBy: u},
		{ID: "ver-shared-" + run, FlowID: shared, CreatedBy: u, Comment: "my edit"},
	} {
		f.ok("SaveFlowVersion", b.SaveFlowVersion(ctx, fv))
	}
	f.ok("SetFlowBaseline", b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: shared, Keys: []string{"r:1"}, CreatedBy: u}))
	// An undo snapshot the user's edit left on someone else's flow (the per-user bound's identity, v47).
	f.ok("PushFlowSnapshot", b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: "snap-shared-" + run, FlowID: shared, UserID: u, Source: "x"},
		interfaces.FlowSnapshotLimits{Keep: 20, MaxBytes: 1 << 20, UserKeep: 200, UserMaxBytes: 1 << 20}))
	for _, st := range []*interfaces.FindingStatus{
		{FlowID: shared, FindingKey: "r:1", RuleID: "r", Status: "acknowledged", AssigneeID: u, UpdatedBy: u},
		{FlowID: orgFlow, FindingKey: "r:2", RuleID: "r", Status: "open", AssigneeID: u, UpdatedBy: v},
		{FlowID: lost, FindingKey: "r:3", RuleID: "r", Status: "acknowledged", UpdatedBy: u},
	} {
		f.ok("SetFindingStatus", b.SetFindingStatus(ctx, st))
	}
	for _, c := range []*interfaces.FindingComment{
		{ID: "c-shared-" + run, FlowID: shared, FindingKey: "r:1", AuthorID: u, AuthorName: name, Body: "kept words"},
		{ID: "c-lost-" + run, FlowID: lost, FindingKey: "r:3", AuthorID: u, AuthorName: email, Body: "on a lost flow"},
		{ID: "c-v-own-" + run, FlowID: own, FindingKey: "r:1", AuthorID: v, AuthorName: "Vic Other", Body: "goes with the flow"},
	} {
		f.ok("AddFindingComment", b.AddFindingComment(ctx, c))
	}
	f.ok("CreateShareToken", b.CreateShareToken(ctx, &interfaces.ShareToken{ID: "st-" + run, FlowID: shared, TokenHash: "sth-" + run, CreatedBy: u}))
	for _, a := range []*interfaces.GovernanceAlert{
		{ID: orgFlow + "|finding_assigned|r:2|" + u, FlowID: orgFlow, Type: "finding_assigned", Title: "t", Message: "Assigned to you by Vic Other", Severity: "info", TargetUser: u},
		{ID: shared + "|comment_added|r:1|c|" + v, FlowID: shared, Type: "comment_added", Title: "t", Message: "New comment from " + name, Severity: "info", TargetUser: v},
		{ID: orgFlow + "|finding_assigned|r:2|" + w, FlowID: orgFlow, Type: "finding_assigned", Title: "t", Message: "Assigned to you by " + email, Severity: "info", TargetUser: w},
		{ID: shared + "|drift", FlowID: shared, Type: "drift", Title: "t", Message: "Health dropped", Severity: "warning"},
		// Messages that are not about the user: another name ending in theirs, and the same name on a flow the user
		// never reached (someone else's namesake).
		{ID: shared + "|comment_added|r:1|c2|" + v, FlowID: shared, Type: "comment_added", Title: "t", Message: "New comment from Big " + name, Severity: "info", TargetUser: v},
		{ID: stranger + "|comment_added|r:1|c|" + v, FlowID: stranger, Type: "comment_added", Title: "t", Message: "New comment from " + name, Severity: "info", TargetUser: v},
	} {
		a.CreatedAt = time.Now().UTC()
		f.ok("RecordGovernanceAlert", b.RecordGovernanceAlert(ctx, a))
	}

	// Organisation rows: invitations to and by the user, a credential configuration they edited (lent by w) and
	// one they lent.
	f.ok("SaveOrgInvite to u", b.SaveOrgInvite(ctx, &interfaces.OrgInvite{ID: "inv-to-" + run, OrgID: orgP, Email: email, Role: auth.RoleMember, InvitedBy: v, TokenHash: "ih1-" + run, ExpiresAt: time.Now().Add(time.Hour)}))
	f.ok("SaveOrgInvite by u", b.SaveOrgInvite(ctx, &interfaces.OrgInvite{ID: "inv-by-" + run, OrgID: orgO, Email: "someone-" + run + "@example.com", Role: auth.RoleMember, InvitedBy: u, TokenHash: "ih2-" + run, ExpiresAt: time.Now().Add(time.Hour)}))
	f.exec(`INSERT INTO org_provider_keys (org_id, provider, ciphertext, updated_by, secret_set_by) VALUES ($1, 'azure-foundry', 'sealed', $2, $3)`, orgO, u, w)
	f.exec(`INSERT INTO org_provider_keys (org_id, provider, ciphertext, updated_by, secret_set_by) VALUES ($1, 'azure-foundry/lent', 'sealed', $2, $2)`, orgO, u)
	// A CI credential the user minted for the organisation they own.
	f.exec(`INSERT INTO org_ci_credentials (id, org_id, name, secret, created_by) VALUES ($1, $2, 'ci', 'enc:v1:sealed', $3)`, "ci-"+run, orgO, u)

	if before := f.scanForIdentity(u, email); len(tablesOf(before)) < 25 {
		t.Fatalf("the seed reaches only %d tables (%v); the sweep would prove little", len(tablesOf(before)), tablesOf(before))
	}

	// Erase as the account route does: inside the request's row-level-security transaction for the user.
	rctx, scope := WithLazyRLS(ctx, b, u, false)
	if err := b.DeleteUser(rctx, u); err != nil {
		_ = scope.Rollback()
		t.Fatalf("DeleteUser: %v", err)
	}
	if err := scope.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	survivors := []erasureSurvivor{
		{"audit_events", "resource_id", "the forensic trail keeps who an action was about (the user's pre-auth rows, another actor's action on the account); the id names no account any more",
			func(r map[string]any) bool { return r["resource_type"] == "user" }},
		{"audit_events", "meta", "another actor's action naming the user (a share, an invitation) is kept as it was recorded",
			func(r map[string]any) bool { return r["user_id"] != "" && r["user_id"] != u }},
		{"org_provider_keys", "updated_by", "a credential the user lent: AccountLifecycleService's finalize disables it after the erasure (DisableOrgAzureCredentialsLentBy)",
			func(r map[string]any) bool { return r["secret_set_by"] == u }},
		{"org_provider_keys", "secret_set_by", "as above: finalize matches the lender by this id",
			func(r map[string]any) bool { return r["secret_set_by"] == u }},
	}
	for _, hit := range f.scanForIdentity(u, email) {
		allowed := false
		for _, s := range survivors {
			if s.table == hit.table && s.column == hit.column && s.match(hit.row) {
				allowed = true
				break
			}
		}
		if !allowed {
			t.Errorf("after erasure %s.%s still names the user: %v", hit.table, hit.column, hit.row)
		}
	}

	// What stays, and how.
	if n := f.count(`SELECT count(*) FROM conversations WHERE flow_id = $1`, own); n != 0 {
		t.Errorf("the owned flow's conversations survived erasure (%d)", n)
	}
	if n := f.count(`SELECT count(*) FROM flows WHERE id = ANY($1)`, []string{shared, orgFlow, lost}); n != 3 {
		t.Errorf("other people's flows: %d left, want 3", n)
	}
	if body := f.str(`SELECT body || '|' || author_id || '|' || author_name FROM finding_comments WHERE id = $1`, "c-shared-"+run); body != "kept words||" {
		t.Errorf("the comment on a shared flow = %q, want its words kept and its author cleared", body)
	}
	if body := f.str(`SELECT body || '|' || author_id FROM finding_comments WHERE id = $1`, "c-lost-"+run); body != "on a lost flow|" {
		t.Errorf("the comment on a flow the user could no longer open = %q, want it anonymised", body)
	}
	if got := f.str(`SELECT assignee_id || '|' || updated_by FROM finding_status WHERE flow_id = $1`, orgFlow); got != "|"+v {
		t.Errorf("the assignment on another's flow = %q, want unassigned and v's decision kept", got)
	}
	if got := f.str(`SELECT message FROM gov_alerts WHERE id = $1`, shared+"|comment_added|r:1|c|"+v); got != "New comment from a teammate" {
		t.Errorf("a teammate's comment alert = %q", got)
	}
	if got := f.str(`SELECT message FROM gov_alerts WHERE id = $1`, orgFlow+"|finding_assigned|r:2|"+w); got != "Assigned to you by a teammate" {
		t.Errorf("a teammate's assignment alert = %q", got)
	}
	if got := f.str(`SELECT message FROM gov_alerts WHERE id = $1`, shared+"|drift"); got != "Health dropped" {
		t.Errorf("an alert not naming the user changed: %q", got)
	}
	if got := f.str(`SELECT message FROM gov_alerts WHERE id = $1`, shared+"|comment_added|r:1|c2|"+v); got != "New comment from Big "+name {
		t.Errorf("an alert naming someone whose name ends in the user's changed: %q", got)
	}
	if got := f.str(`SELECT message FROM gov_alerts WHERE id = $1`, stranger+"|comment_added|r:1|c|"+v); got != "New comment from "+name {
		t.Errorf("another tenant's alert about a namesake changed: %q", got)
	}
	if got := f.str(`SELECT owner_id || '|' || (SELECT count(*) FROM org_members WHERE org_id = $1) FROM organisations WHERE id = $1`, orgO); got != "|1" {
		t.Errorf("the organisation the user owned = %q, want it kept with its other member and no owner", got)
	}
	if n := f.count(`SELECT count(*) FROM org_invites WHERE id = $1 AND invited_by = ''`, "inv-by-"+run); n != 1 {
		t.Error("the invitation the user sent must stay, without the inviter")
	}
	if got := f.str(`SELECT updated_by || '|' || secret_set_by FROM org_provider_keys WHERE org_id = $1 AND provider = 'azure-foundry'`, orgO); got != "|"+w {
		t.Errorf("the credential the user edited and w lent = %q, want the editor cleared and the lender kept", got)
	}
	if n := f.count(`SELECT count(*) FROM org_ci_credentials WHERE id = $1 AND created_by = ''`, "ci-"+run); n != 1 {
		t.Error("the CI credential the user minted must stay the organisation's, without its minter")
	}
	if n := f.count(`SELECT count(*) FROM flow_snapshots WHERE id = $1 AND user_id = ''`, "snap-shared-"+run); n != 1 {
		t.Error("the undo snapshot the user left on another's flow must stay the flow's, without the user")
	}
	if got := f.str(`SELECT ip FROM audit_events WHERE id = $1`, "ev-pre-"+run); got != "203.0.113.3" {
		t.Errorf("the pre-auth row's address = %q, kept as before", got)
	}
	if got := f.str(`SELECT resource_id || '|' || ip || '|' || COALESCE(user_agent, '<null>') FROM audit_events WHERE id = $1`, "ev-pre-email-"+run); got != "|203.0.113.4|<null>" {
		t.Errorf("the failed login naming the user's email = %q, want the email and user agent gone, the address kept", got)
	}

	// Idempotent.
	if err := b.DeleteUser(ctx, u); err != nil {
		t.Errorf("a second erasure: %v", err)
	}
}

func tablesOf(hits []identityHit) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range hits {
		if !seen[h.table] {
			seen[h.table] = true
			out = append(out, h.table)
		}
	}
	return out
}

// TestPostgres_Lifecycle_DeleteOrgSweep deletes an organisation with configuration, content, flows owned by two
// members and history rows, inside an admin's row-level-security scope, and checks that no row keyed by it
// survives outside the tables that keep the id as history, and that its flows now belong to their owners alone.
func TestPostgres_Lifecycle_DeleteOrgSweep(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	f := lifecycleFixture{t: t, b: b, ctx: ctx}
	run := fmt.Sprintf("%x", time.Now().UnixNano())

	admin, member := "orgdel-admin-"+run, "orgdel-member-"+run
	for _, id := range []string{admin, member} {
		f.ok("SaveUser", b.SaveUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "x"}))
	}
	org := "orgdel-" + run
	f.ok("SaveOrg", b.SaveOrg(ctx, &interfaces.Organisation{ID: org, Name: "Doomed", OwnerID: admin,
		Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}, {UserID: member, Role: auth.RoleMember}}}))
	adminFlow, memberFlow := "orgdel-flow-a-"+run, "orgdel-flow-m-"+run
	f.ok("SaveFlow", b.SaveFlow(ctx, &interfaces.FlowDocument{ID: adminFlow, Name: "a", OwnerID: admin, OrganizationID: org, Content: json.RawMessage(`{}`)}))
	f.ok("SaveFlow", b.SaveFlow(ctx, &interfaces.FlowDocument{ID: memberFlow, Name: "m", OwnerID: member, OrganizationID: org, Content: json.RawMessage(`{}`)}))

	f.ok("SaveOrgSettings", b.SaveOrgSettings(ctx, org, &interfaces.AppSettings{Version: 1}))
	f.ok("SaveOrgInvite", b.SaveOrgInvite(ctx, &interfaces.OrgInvite{ID: "oi-" + run, OrgID: org, Email: "x-" + run + "@example.com", Role: auth.RoleMember, InvitedBy: admin, TokenHash: "oih-" + run, ExpiresAt: time.Now().Add(time.Hour)}))
	f.exec(`INSERT INTO org_provider_keys (org_id, provider, ciphertext, updated_by, secret_set_by) VALUES ($1, 'azure-foundry', 'sealed', $2, $2)`, org, admin)
	_, err := NewPostgresAzureRegistry(b).CreateConnection(ctx, interfaces.AzureOrgOwner(org), interfaces.AzureConnectionRecord{ID: "default", Name: "shared"})
	f.ok("CreateConnection", err)
	f.exec(`INSERT INTO org_azure_registry_state (org_id) VALUES ($1)`, org)
	f.exec(`INSERT INTO policies (id, org_id, name) VALUES ($1, $2, 'gate')`, "pol-"+run, org)
	f.exec(`INSERT INTO org_channels (id, org_id, name, kind, url) VALUES ($1, $2, 'hook', 'webhook', 'https://example.com/hook')`, "ch-"+run, org)
	f.exec(`INSERT INTO org_custom_rules (id, org_id, rule_id, config) VALUES ($1, $2, 'r', '{}')`, "cr-"+run, org)
	f.exec(`INSERT INTO org_ci_credentials (id, org_id, name, secret, created_by) VALUES ($1, $2, 'ci', 'enc:v1:sealed', $3)`, "ci-"+run, org, admin)
	f.exec(`INSERT INTO knowledge_documents (id, org_id, filename) VALUES ($1, $2, 'guide.md')`, "kd-"+run, org)
	f.exec(`INSERT INTO knowledge_chunks (id, doc_id, content, embedding, org_id) VALUES ($1, $2, 'text', '[]', $3)`, "kc-"+run, "kd-"+run, org)
	// History that keeps the id.
	f.ok("SaveUsageMetric", b.SaveUsageMetric(ctx, &interfaces.UsageMetric{ID: "ou-" + run, UserID: member, OrgID: org, Provider: "openai", Model: "m"}))
	f.ok("RecordGovernanceAlert", b.RecordGovernanceAlert(ctx, &interfaces.GovernanceAlert{ID: memberFlow + "|drift", FlowID: memberFlow, OrgID: org, Type: "drift", Title: "t", Severity: "warning", CreatedAt: time.Now().UTC()}))
	f.ok("SaveAuditEvent", b.SaveAuditEvent(ctx, &interfaces.AuditEvent{ID: "oa-" + run, UserID: admin, Action: "org.update", ResourceType: "org", ResourceID: org, OrgID: org, CreatedAt: time.Now().UTC()}))

	// As the org route does: the admin's request transaction. The member's flow is not the admin's to update
	// under the flows policy, so this also proves the deletion runs without the row-level-security user.
	rctx, scope := WithLazyRLS(ctx, b, admin, false)
	if err := b.DeleteOrg(rctx, org); err != nil {
		_ = scope.Rollback()
		t.Fatalf("DeleteOrg: %v", err)
	}
	if err := scope.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	claims := map[string]tableClaim{}
	for _, c := range tableClaims() {
		claims[c.table] = c
	}
	keyed, err := rowsOfStrings(ctx, b.db, `SELECT table_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND column_name = 'org_id' ORDER BY 1`)
	f.ok("org-keyed tables", err)
	for _, table := range keyed {
		n := f.count(fmt.Sprintf(`SELECT count(*) FROM %s WHERE org_id = $1`, table), org)
		switch kept := claims[table].org == orgKept; {
		case kept && n == 0:
			t.Errorf("%s keeps the organisation's id as history, but its seeded row is gone", table)
		case !kept && n != 0:
			t.Errorf("%s still has %d row(s) of the deleted organisation", table, n)
		}
	}
	if n := f.count(`SELECT count(*) FROM organisations WHERE id = $1`, org); n != 0 {
		t.Error("the organisation row survived")
	}
	if n := f.count(`SELECT count(*) FROM knowledge_chunks WHERE id = $1`, "kc-"+run); n != 0 {
		t.Error("the knowledge chunks survived their document")
	}
	if n := f.count(`SELECT count(*) FROM flows WHERE id = ANY($1) AND org_id = ''`, []string{adminFlow, memberFlow}); n != 2 {
		t.Errorf("%d of the organisation's 2 flows were detached to their owners", n)
	}
	// A missing organisation is not an error.
	if err := b.DeleteOrg(ctx, org); err != nil {
		t.Errorf("deleting it again: %v", err)
	}
}
