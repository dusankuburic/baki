package database

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// Tenant scoping of the id-only methods (ADR 0013): inside a user's RLS scope the flow-child tables, flow_analysis
// and share_tokens since v34, and the collaborator statements reach only the rows of flows the caller can see.
// DATABASE_URL-gated (throwaway DB).

// scopedFixture is one owner's flow with a row in every scoped table, a stranger who cannot see it and a viewer
// collaborator who can.
type scopedFixture struct {
	*r2Fixture
	owner, viewer, stranger string
	flow, key               string
	commentID               string
	shareID, shareHash      string
}

func newScopedFixture(t *testing.T) *scopedFixture {
	t.Helper()
	f := &scopedFixture{r2Fixture: newR2Fixture(t)}
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 34 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	skipIfRLSBypassedInternal(t, f.b)
	f.owner, f.viewer, f.stranger = f.user("owner"), f.user("viewer"), f.user("stranger")
	f.flow = f.r2Fixture.flow("flow", f.owner, "", "Scoped", "")
	f.collab(f.flow, f.viewer)
	f.key = "rule-x:" + f.run

	// Seeded on the pool, as the owner's requests and the background jobs would have written them.
	b, ctx := f.b, f.ctx
	if err := b.SaveFlowAnalysis(ctx, &interfaces.FlowAnalysis{FlowID: f.flow, HealthScore: 80, AnalyzedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("SaveFlowAnalysis: %v", err)
	}
	if err := b.SetFindingStatus(ctx, &interfaces.FindingStatus{FlowID: f.flow, FindingKey: f.key, Status: "acknowledged", UpdatedBy: f.owner}); err != nil {
		t.Fatalf("SetFindingStatus: %v", err)
	}
	if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: f.flow, Keys: []string{f.key}, CreatedBy: f.owner}); err != nil {
		t.Fatalf("SetFlowBaseline: %v", err)
	}
	c := &interfaces.FindingComment{FlowID: f.flow, FindingKey: f.key, AuthorID: f.owner, AuthorName: "Owner", Body: "note"}
	if err := b.AddFindingComment(ctx, c); err != nil {
		t.Fatalf("AddFindingComment: %v", err)
	}
	f.commentID = c.ID
	f.shareHash = "hash-" + f.run
	st := &interfaces.ShareToken{FlowID: f.flow, TokenHash: f.shareHash, CreatedBy: f.owner}
	if err := b.CreateShareToken(ctx, st); err != nil {
		t.Fatalf("CreateShareToken: %v", err)
	}
	f.shareID = st.ID
	return f
}

// as runs fn in userID's own RLS transaction and commits it, so a write the policies let through is there
// afterwards (a transaction a refused write aborted rolls back on the commit).
func (f *scopedFixture) as(userID string, fn func(ctx context.Context)) {
	f.t.Helper()
	tx, err := f.b.BeginRLS(f.ctx, userID)
	if err != nil {
		f.t.Fatalf("BeginRLS: %v", err)
	}
	defer func() { _ = tx.Commit() }()
	fn(WithRLSTx(f.ctx, tx))
}

// rows counts the rows of table matching where, on the pool (every row).
func (f *scopedFixture) rows(table, where string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.b.db.QueryRowContext(f.ctx, `SELECT count(*) FROM `+table+` WHERE `+where, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestRLS_ScopedMethods_StrangerReachesNothing: a stranger in their own RLS scope reads none of the owner's rows
// through the id-only methods, and every write against the owner's flow is refused or matches nothing.
func TestRLS_ScopedMethods_StrangerReachesNothing(t *testing.T) {
	f := newScopedFixture(t)
	b, s := f.b, f.stranger

	f.as(s, func(ctx context.Context) {
		if h, err := b.LoadFlowHealth(ctx, f.flow); err != nil || h != nil {
			t.Errorf("LoadFlowHealth = %+v, %v; want nil, nil", h, err)
		}
		if m, err := b.LoadFlowHealthBatch(ctx, []string{f.flow}); err != nil || len(m) != 0 {
			t.Errorf("LoadFlowHealthBatch = %v, %v; want empty", m, err)
		}
		if st, err := b.GetFindingStatus(ctx, f.flow, f.key); err != nil || st != nil {
			t.Errorf("GetFindingStatus = %+v, %v; want nil, nil", st, err)
		}
		if l, err := b.ListFindingStatuses(ctx, f.flow); err != nil || len(l) != 0 {
			t.Errorf("ListFindingStatuses = %d rows, %v; want none", len(l), err)
		}
		if bl, err := b.GetFlowBaseline(ctx, f.flow); err != nil || bl != nil {
			t.Errorf("GetFlowBaseline = %+v, %v; want nil, nil", bl, err)
		}
		if m, err := b.GetFlowBaselinesBatch(ctx, []string{f.flow}); err != nil || len(m) != 0 {
			t.Errorf("GetFlowBaselinesBatch = %v, %v; want empty", m, err)
		}
		if l, err := b.ListFindingComments(ctx, f.flow, f.key); err != nil || len(l) != 0 {
			t.Errorf("ListFindingComments = %d rows, %v; want none", len(l), err)
		}
		if l, err := b.ListShareTokens(ctx, f.flow); err != nil || len(l) != 0 {
			t.Errorf("ListShareTokens = %d rows, %v; want none", len(l), err)
		}
		if _, err := b.GetShareTokenByHash(ctx, f.shareHash); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("GetShareTokenByHash in the stranger's scope = %v, want ErrNotFound", err)
		}
		if l, err := b.ListCollaborators(ctx, f.flow, 0); err != nil || len(l) != 0 {
			t.Errorf("ListCollaborators = %d rows, %v; want none", len(l), err)
		}
		if m, err := b.ListCollaboratorsBatch(ctx, []string{f.flow}); err != nil || len(m) != 0 {
			t.Errorf("ListCollaboratorsBatch = %v, %v; want empty", m, err)
		}
	})

	// Each refused write aborts its transaction, so each gets its own.
	refused := map[string]func(ctx context.Context) error{
		"SaveFlowAnalysis": func(ctx context.Context) error {
			return b.SaveFlowAnalysis(ctx, &interfaces.FlowAnalysis{FlowID: f.flow, HealthScore: 1})
		},
		"SetFindingStatus": func(ctx context.Context) error {
			return b.SetFindingStatus(ctx, &interfaces.FindingStatus{FlowID: f.flow, FindingKey: f.key, Status: "resolved", UpdatedBy: s})
		},
		"BatchSetFindingStatus": func(ctx context.Context) error {
			return b.BatchSetFindingStatus(ctx, f.flow, s, []*interfaces.FindingStatus{{FindingKey: f.key, Status: "resolved", UpdatedBy: s}})
		},
		"SetFlowBaseline": func(ctx context.Context) error {
			return b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: f.flow, Keys: []string{}, CreatedBy: s})
		},
		"AddFindingComment": func(ctx context.Context) error {
			return b.AddFindingComment(ctx, &interfaces.FindingComment{FlowID: f.flow, FindingKey: f.key, AuthorID: s, Body: "x"})
		},
		"CreateShareToken": func(ctx context.Context) error {
			return b.CreateShareToken(ctx, &interfaces.ShareToken{FlowID: f.flow, TokenHash: "stranger-" + f.run, CreatedBy: s})
		},
		"AddCollaborator": func(ctx context.Context) error {
			return b.AddCollaborator(ctx, f.flow, &interfaces.Collaborator{UserID: s, Permission: "admin"})
		},
		"UpdateCollaborator": func(ctx context.Context) error {
			return b.UpdateCollaborator(ctx, f.flow, f.viewer, "admin")
		},
		"RemoveCollaborator": func(ctx context.Context) error {
			return b.RemoveCollaborator(ctx, f.flow, f.viewer)
		},
	}
	for _, name := range sortedKeys(refused) {
		f.as(s, func(ctx context.Context) {
			if err := refused[name](ctx); err == nil {
				t.Errorf("%s on another tenant's flow succeeded", name)
			}
		})
	}
	// BatchSetFindingStatus opens its own scope for the user when the caller has none.
	if err := b.BatchSetFindingStatus(f.ctx, f.flow, s, []*interfaces.FindingStatus{{FindingKey: f.key, Status: "resolved", UpdatedBy: s}}); err == nil {
		t.Error("BatchSetFindingStatus in its own scope for the stranger succeeded")
	}

	// The deletes match nothing: no error, nothing gone.
	f.as(s, func(ctx context.Context) {
		for name, err := range map[string]error{
			"DeleteFindingStatus":             b.DeleteFindingStatus(ctx, f.flow, f.key),
			"ClearFlowBaseline":               b.ClearFlowBaseline(ctx, f.flow),
			"DeleteFindingComment":            b.DeleteFindingComment(ctx, f.flow, f.commentID, s),
			"DeleteFindingCommentAsModerator": b.DeleteFindingCommentAsModerator(ctx, f.flow, f.commentID),
			"RevokeShareToken":                b.RevokeShareToken(ctx, f.flow, f.shareID),
		} {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	for _, c := range []struct {
		table, where string
		args         []any
		want         int
	}{
		{"flow_analysis", "flow_id = $1 AND health_score = 80", []any{f.flow}, 1},
		{"finding_status", "flow_id = $1 AND finding_key = $2 AND status = 'acknowledged'", []any{f.flow, f.key}, 1},
		{"flow_baselines", "flow_id = $1 AND created_by = $2", []any{f.flow, f.owner}, 1},
		{"finding_comments", "flow_id = $1", []any{f.flow}, 1},
		{"share_tokens", "flow_id = $1", []any{f.flow}, 1},
		{"flow_collaborators", "flow_id = $1 AND user_id = $2 AND permission = 'viewer'", []any{f.flow, f.viewer}, 1},
		{"flow_collaborators", "flow_id = $1 AND user_id = $2", []any{f.flow, s}, 0},
	} {
		if got := f.rows(c.table, c.where, c.args...); got != c.want {
			t.Errorf("%s where %s: %d rows after the stranger's writes, want %d", c.table, c.where, got, c.want)
		}
	}

	// The anonymous share-link lookup runs without a user context and still resolves the token.
	if st, err := b.GetShareTokenByHash(f.ctx, f.shareHash); err != nil || st.ID != f.shareID {
		t.Errorf("anonymous GetShareTokenByHash = %+v, %v; want the owner's link", st, err)
	}
}

// TestRLS_ScopedMethods_VisibleFlowStaysReachable: the scoping is visibility, nothing stricter. The owner reaches
// every row, and a viewer collaborator reads them and may still write at this layer: the editor check of the
// triage, comment and share-link writes is the service's (ADR 0013), and a row-level edit check would also block
// account erasure, which runs in the departing user's scope and rewrites their identity on flows they only view.
func TestRLS_ScopedMethods_VisibleFlowStaysReachable(t *testing.T) {
	f := newScopedFixture(t)
	b := f.b
	for _, u := range []string{f.owner, f.viewer} {
		f.as(u, func(ctx context.Context) {
			if h, err := b.LoadFlowHealth(ctx, f.flow); err != nil || h == nil || h.HealthScore != 80 {
				t.Errorf("%s: LoadFlowHealth = %+v, %v", u, h, err)
			}
			if m, err := b.LoadFlowHealthBatch(ctx, []string{f.flow}); err != nil || m[f.flow] == nil {
				t.Errorf("%s: LoadFlowHealthBatch = %v, %v", u, m, err)
			}
			if st, err := b.GetFindingStatus(ctx, f.flow, f.key); err != nil || st == nil {
				t.Errorf("%s: GetFindingStatus = %+v, %v", u, st, err)
			}
			if m, err := b.GetFlowBaselinesBatch(ctx, []string{f.flow}); err != nil || m[f.flow] == nil {
				t.Errorf("%s: GetFlowBaselinesBatch = %v, %v", u, m, err)
			}
			if l, err := b.ListFindingComments(ctx, f.flow, f.key); err != nil || len(l) != 1 {
				t.Errorf("%s: ListFindingComments = %d rows, %v", u, len(l), err)
			}
			if l, err := b.ListShareTokens(ctx, f.flow); err != nil || len(l) != 1 {
				t.Errorf("%s: ListShareTokens = %d rows, %v", u, len(l), err)
			}
			if l, err := b.ListCollaborators(ctx, f.flow, 0); err != nil || len(l) != 1 {
				t.Errorf("%s: ListCollaborators = %d rows, %v", u, len(l), err)
			}
		})
	}

	f.as(f.viewer, func(ctx context.Context) {
		if err := b.SetFindingStatus(ctx, &interfaces.FindingStatus{FlowID: f.flow, FindingKey: f.key, Status: "resolved", UpdatedBy: f.viewer}); err != nil {
			t.Errorf("viewer SetFindingStatus refused by the policy: %v", err)
		}
		// A viewer may leave a flow: the self-removal reaches the grant through the flow they still see.
		if err := b.RemoveCollaborator(ctx, f.flow, f.viewer); err != nil {
			t.Errorf("viewer RemoveCollaborator (self) = %v", err)
		}
	})
	f.as(f.owner, func(ctx context.Context) {
		if err := b.SaveFlowAnalysis(ctx, &interfaces.FlowAnalysis{FlowID: f.flow, HealthScore: 90}); err != nil {
			t.Errorf("owner SaveFlowAnalysis: %v", err)
		}
		if err := b.CreateShareToken(ctx, &interfaces.ShareToken{FlowID: f.flow, TokenHash: "owner-" + f.run, CreatedBy: f.owner}); err != nil {
			t.Errorf("owner CreateShareToken: %v", err)
		}
		if err := b.AddCollaborator(ctx, f.flow, &interfaces.Collaborator{UserID: f.stranger, Permission: "editor"}); err != nil {
			t.Errorf("owner AddCollaborator: %v", err)
		}
		if err := b.DeleteFindingCommentAsModerator(ctx, f.flow, f.commentID); err != nil {
			t.Errorf("owner DeleteFindingCommentAsModerator: %v", err)
		}
	})
	for _, c := range []struct {
		table, where string
		args         []any
		want         int
	}{
		{"finding_status", "flow_id = $1 AND status = 'resolved'", []any{f.flow}, 1},
		{"flow_collaborators", "flow_id = $1 AND user_id = $2", []any{f.flow, f.viewer}, 0},
		{"flow_collaborators", "flow_id = $1 AND user_id = $2 AND permission = 'editor'", []any{f.flow, f.stranger}, 1},
		{"flow_analysis", "flow_id = $1 AND health_score = 90", []any{f.flow}, 1},
		{"share_tokens", "flow_id = $1", []any{f.flow}, 2},
		{"finding_comments", "flow_id = $1", []any{f.flow}, 0},
	} {
		if got := f.rows(c.table, c.where, c.args...); got != c.want {
			t.Errorf("%s where %s: %d rows, want %d", c.table, c.where, got, c.want)
		}
	}
}

// TestCollaboratorWrites_MissingFlowNotFound: the collaborator writes answer ErrNotFound for a flow that does not
// exist, with or without a scope, instead of a foreign-key error.
func TestCollaboratorWrites_MissingFlowNotFound(t *testing.T) {
	f := newR2Fixture(t)
	u := f.user("grantee")
	missing := f.id("missing-flow")
	if err := f.b.AddCollaborator(f.ctx, missing, &interfaces.Collaborator{UserID: u, Permission: "viewer"}); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("AddCollaborator on a missing flow = %v, want ErrNotFound", err)
	}
	if err := f.b.UpdateCollaborator(f.ctx, missing, u, "editor"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("UpdateCollaborator on a missing flow = %v, want ErrNotFound", err)
	}
	if err := f.b.RemoveCollaborator(f.ctx, missing, u); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("RemoveCollaborator on a missing flow = %v, want ErrNotFound", err)
	}
}

// TestFindingComments_DeleteNeedsAnAuthor: an empty author matches no comment, so it can never mean "anyone";
// the moderator's delete is its own method.
func TestFindingComments_DeleteNeedsAnAuthor(t *testing.T) {
	f := newR2Fixture(t)
	owner, other := f.user("author"), f.user("other")
	flow := f.flow("comments", owner, "", "Comments", "")
	add := func(author string) string {
		c := &interfaces.FindingComment{FlowID: flow, FindingKey: "k", AuthorID: author, Body: "b"}
		if err := f.b.AddFindingComment(f.ctx, c); err != nil {
			t.Fatalf("AddFindingComment: %v", err)
		}
		return c.ID
	}
	own, others := add(owner), add(other)
	left := func() []string {
		cs, err := f.b.ListFindingComments(f.ctx, flow, "k")
		if err != nil {
			t.Fatalf("ListFindingComments: %v", err)
		}
		var ids []string
		for _, c := range cs {
			ids = append(ids, c.ID)
		}
		sort.Strings(ids)
		return ids
	}
	if err := f.b.DeleteFindingComment(f.ctx, flow, others, ""); !errors.Is(err, interfaces.ErrNotCommentAuthor) {
		t.Errorf("delete with no author = %v, want ErrNotCommentAuthor", err)
	}
	if err := f.b.DeleteFindingComment(f.ctx, flow, others, owner); !errors.Is(err, interfaces.ErrNotCommentAuthor) {
		t.Errorf("delete of someone else's comment = %v, want ErrNotCommentAuthor", err)
	}
	if got, want := strings.Join(left(), ","), strings.Join(sortedCopy([]string{own, others}), ","); got != want {
		t.Fatalf("comments left = %s, want both (%s)", got, want)
	}
	if err := f.b.DeleteFindingComment(f.ctx, flow, own, owner); err != nil {
		t.Errorf("delete of one's own comment: %v", err)
	}
	if err := f.b.DeleteFindingCommentAsModerator(f.ctx, flow, others); err != nil {
		t.Errorf("moderator delete: %v", err)
	}
	if got := left(); len(got) != 0 {
		t.Errorf("comments left = %v, want none", got)
	}
	if err := f.b.DeleteFindingCommentAsModerator(f.ctx, flow, others); err != nil {
		t.Errorf("moderator delete of an absent comment = %v, want nil", err)
	}
}

// TestRLS_V34_CatalogBodies: flow_analysis and share_tokens each have exactly their one FOR ALL policy, delegating to
// the flows policy like the v27 flow-child tables.
func TestRLS_V34_CatalogBodies(t *testing.T) {
	b := openMigrationTestDB(t)
	if v, _ := b.CurrentSchemaVersion(context.Background()); v < 34 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	for table, name := range map[string]string{"flow_analysis": "rls_flow_analysis_visible", "share_tokens": "rls_share_tokens_visible"} {
		pols := tablePolicies(t, b, table)
		if len(pols) != 1 || pols[0].name != name || pols[0].cmd != "ALL" {
			t.Errorf("%s policies = %+v, want %s FOR ALL", table, pols, name)
			continue
		}
		for _, body := range []string{pols[0].qual, pols[0].check} {
			if !strings.Contains(body, "app_rls_active()") || !strings.Contains(body, "f.id = "+table+".flow_id") {
				t.Errorf("%s policy body does not delegate to flows: %q", table, body)
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tablesWithoutRLS is every table that deliberately has no row-level security, with the reason (ADR 0013). A new
// table gets a policy, or a line here saying why every statement on it is scoped without one.
var tablesWithoutRLS = map[string]string{
	"schema_migrations":  "migration bookkeeping; no request reads it",
	"users":              "auth: looked up by email or id before a user context exists; the other reads are lookups a service authorizes (the caller's own row, the admin routes, resolving a user to share a flow with)",
	"api_tokens":         "auth: resolved by hash before the user is known; every other statement filters by user_id",
	"user_tokens":        "auth: one-shot tokens resolved by hash before the user is known",
	"refresh_tokens":     "auth: resolved by hash before the user is known; every other statement filters by user_id",
	"token_blacklist":    "auth: revoked token ids checked before the user is known",
	"identity_links":     "auth: an SSO subject resolved to its user before the user is known",
	"org_members":        "the grant table every org-scoped policy reads: a policy on it would recurse",
	"flow_collaborators": "the grant table every flow policy reads: a policy on it would recurse; its statements reach grants through the flows policy instead",
	"organisations":      "read by the authorization checks themselves (an unknown org answers differently from a refusal), by invite acceptance before the invitee is a member and by the realtime roster at delivery; every caller is authorized at the service",
	"org_invites":        "accepted by token hash before the invitee is a member; the other statements filter by org_id behind an org-admin check",
	"flow_search":        "derived from flows and only read through them (id IN (SELECT flow_id FROM flow_search ...)); a policy would make its trigram indexes unusable",
	"app_settings":       "the deployment's single settings row",
	"user_settings":      "keyed by the caller's own user id",
	"org_settings":       "keyed by org id behind an org read or admin check",
	"settings_sections":  "the per-section copies of the three settings documents, keyed like them",
	"audit_events":       "written for every actor, read by the admin route and the data-subject paths with explicit predicates",
	"usage_metrics":      "written by the AI usage meter, read with explicit user_id or org_id predicates",
	"provider_keys":      "keystore: sealed secrets keyed by their owner's id, on the keystore's own connection",
	"org_provider_keys":  "keystore: sealed secrets keyed by org id, on the keystore's own connection",
	"job_runs":           "background job state: written and read by the jobs on the pool, never inside a request; no tenant's data",
}

// TestRLS_TablesWithoutRowSecurityAreListed: every table of the schema has row-level security or is listed in
// tablesWithoutRLS, so a new table cannot ship unscoped by omission. A catalog query: it runs as a superuser too.
func TestRLS_TablesWithoutRowSecurityAreListed(t *testing.T) {
	b := openMigrationTestDB(t)
	if v, _ := b.CurrentSchemaVersion(context.Background()); v < 34 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	rows, err := b.db.QueryContext(context.Background(), `
		SELECT c.relname, c.relrowsecurity
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p')
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("query pg_class: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		var rls bool
		if err := rows.Scan(&name, &rls); err != nil {
			t.Fatal(err)
		}
		seen[name] = true
		_, listed := tablesWithoutRLS[name]
		switch {
		case !rls && !listed:
			t.Errorf("table %s has no row-level security: give it a policy, or list it in tablesWithoutRLS with the reason", name)
		case rls && listed:
			t.Errorf("table %s has row-level security now: remove it from tablesWithoutRLS", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range sortedKeys(tablesWithoutRLS) {
		if !seen[name] {
			t.Errorf("tablesWithoutRLS lists %s, which the schema no longer has", name)
		}
	}
}
