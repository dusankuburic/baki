package database

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// Unit tests for the 2026-10 storage performance round. They run against the
// fakeSQL driver (fakesql_test.go): no Postgres is involved, so they pin the
// statements the backend SENDS and the connection it uses. Whether Postgres
// plans those statements well (the v26 indexes) can only be verified against
// a real server.

// TestLoadFlowHeader_SelectsNeitherContentNorSource: the header read backs
// every permission check, every ResolveDoc cache check and every OCC version
// read. It used to run the full row select (content + source) and drop the
// content afterwards, so each "header" transferred the whole document twice.
func TestLoadFlowHeader_SelectsNeitherContentNorSource(t *testing.T) {
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	b, f := newFakeBackend(t, 1, func(query string, _ []driver.NamedValue) fakeResult {
		return fakeResult{
			cols: []string{"id", "name", "description", "metadata", "owner_id", "org_id", "created_at", "updated_at", "version", "tags"},
			rows: [][]driver.Value{{"f1", "Flow", "desc", []byte(`{"BlockCount":7}`), "u1", "o1", created, created, int64(4), "prod,crit"}},
		}
	})

	hdr, err := b.LoadFlowHeader(context.Background(), "f1")
	if err != nil {
		t.Fatalf("LoadFlowHeader: %v", err)
	}
	qs := f.Queries()
	if len(qs) != 1 {
		t.Fatalf("expected one statement, got %d", len(qs))
	}
	for _, col := range selectList(qs[0].query) {
		if col == "content" || col == "source" {
			t.Errorf("header query selects %q — it must not transfer the document bodies:\n%s", col, oneLine(qs[0].query))
		}
	}
	if hdr.ID != "f1" || hdr.OwnerID != "u1" || hdr.OrganizationID != "o1" || hdr.Version != 4 {
		t.Errorf("header fields not scanned: %+v", hdr)
	}
	if hdr.Metadata.BlockCount != 7 || len(hdr.Tags) != 2 {
		t.Errorf("metadata/tags not scanned: %+v %v", hdr.Metadata, hdr.Tags)
	}
	if hdr.Content != nil || hdr.Source != "" {
		t.Errorf("header must carry no bodies: content=%d bytes, source=%d bytes", len(hdr.Content), len(hdr.Source))
	}
}

func TestLoadFlowHeader_MissingIsNotFound(t *testing.T) {
	b, _ := newFakeBackend(t, 1, nil)
	if _, err := b.LoadFlowHeader(context.Background(), "nope"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Fatalf("LoadFlowHeader(missing) = %v, want ErrNotFound", err)
	}
}

// TestReads_UseTheRequestTransaction is the regression gate for the
// second-connection hazard: the RLS middleware pins ONE pooled connection for
// the request, and these reads used b.db directly, so each took a SECOND one.
// With the pool capped at 1 that is deterministic: a read on b.db can only
// wait for the connection the transaction holds and times out, a read that
// joins the transaction runs at once. (Production pool: 25 — about 13
// concurrent requests deadlocked until the 30 s timeout.)
func TestReads_UseTheRequestTransaction(t *testing.T) {
	cases := []struct {
		name string
		call func(ctx context.Context, b *PostgresStorageBackend) error
	}{
		{"LoadUserByEmail", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.LoadUserByEmail(ctx, "a@example.test")
			return err
		}},
		{"LoadUserByID", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.LoadUserByID(ctx, "u1")
			return err
		}},
		{"LoadUsersByIDs", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.LoadUsersByIDs(ctx, []string{"u1", "u2"})
			return err
		}},
		{"CountUsers", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.CountUsers(ctx)
			return err
		}},
		{"ListUsers", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.ListUsers(ctx, 10, 0)
			return err
		}},
		{"ListAdmins", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.ListAdmins(ctx)
			return err
		}},
		{"IsRefreshTokenValid", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.IsRefreshTokenValid(ctx, "jti")
			return err
		}},
		{"ListUserRefreshTokens", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.ListUserRefreshTokens(ctx, "u1")
			return err
		}},
		{"ListAuditEvents", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.ListAuditEvents(ctx, interfaces.AuditFilter{UserID: "u1"})
			return err
		}},
		{"GetDailyUsage(user)", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.GetDailyUsage(ctx, "u1", "")
			return err
		}},
		{"GetDailyUsage(org)", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.GetDailyUsage(ctx, "u1", "o1")
			return err
		}},
		{"GetDailyUsage(local user)", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.GetDailyUsage(ctx, "", "")
			return err
		}},
		{"LoadIdentityLink", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.LoadIdentityLink(ctx, "oidc", "sub")
			return err
		}},
		{"ListIdentityLinks", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.ListIdentityLinks(ctx, "u1")
			return err
		}},
		{"ListOrgInvites", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.ListOrgInvites(ctx, "o1")
			return err
		}},
		{"GetOrgInvite", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.GetOrgInvite(ctx, "o1", "i1")
			return err
		}},
		{"GetOrgInviteByTokenHash", func(ctx context.Context, b *PostgresStorageBackend) error {
			_, err := b.GetOrgInviteByTokenHash(ctx, "hash")
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, f := newFakeBackend(t, 1, nil)
			tx, err := b.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			ctx, cancel := context.WithTimeout(WithRLSTx(context.Background(), tx), 500*time.Millisecond)
			defer cancel()

			err = c.call(ctx, b)
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s waited for a second pooled connection while the request transaction held the only one", c.name)
			}
			qs := f.Queries()
			if len(qs) == 0 {
				t.Fatalf("%s sent no statement (err=%v)", c.name, err)
			}
			for _, q := range qs {
				if !q.inTx || q.conn != 1 {
					t.Errorf("%s ran %q outside the request transaction (conn %d, inTx %v)", c.name, oneLine(q.query), q.conn, q.inTx)
				}
			}
		})
	}
}

// TestCreateUser_ChecksEmptinessWithExists: registration asks "is this the
// first user" (bootstrap admin), which a COUNT(*) answered by reading every
// row on every registration.
func TestCreateUser_ChecksEmptinessWithExists(t *testing.T) {
	for _, existing := range []bool{false, true} {
		b, f := newFakeBackend(t, 1, func(query string, _ []driver.NamedValue) fakeResult {
			if strings.Contains(query, "EXISTS") {
				return fakeResult{cols: []string{"exists"}, rows: [][]driver.Value{{existing}}}
			}
			return fakeResult{}
		})
		u := &interfaces.User{ID: "u1", Email: "a@example.test", Role: auth.RoleMember}
		if err := b.CreateUser(auth.WithAllowBootstrap(context.Background(), true), u); err != nil {
			t.Fatalf("CreateUser(existing=%v): %v", existing, err)
		}
		for _, q := range f.Queries() {
			if strings.Contains(strings.ToUpper(q.query), "COUNT(") {
				t.Errorf("CreateUser counts the users table: %s", oneLine(q.query))
			}
		}
		wantRole := auth.RoleAdmin
		if existing {
			wantRole = auth.RoleMember
		}
		if u.Role != wantRole {
			t.Errorf("existing=%v: role = %q, want %q (bootstrap admin only on an empty table)", existing, u.Role, wantRole)
		}
	}
}

// TestOrgMemberRole_OnePointLookup: org membership checks (every flow authz
// on an org flow) loaded the org row plus its WHOLE roster.
func TestOrgMemberRole_OnePointLookup(t *testing.T) {
	cases := []struct {
		name       string
		orgExists  bool
		role       driver.Value
		wantRole   auth.Role
		wantMember bool
		wantErr    error
	}{
		{"member", true, "admin", auth.RoleAdmin, true, nil},
		{"not a member", true, nil, "", false, nil},
		{"no such org", false, nil, "", false, interfaces.ErrNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, f := newFakeBackend(t, 1, func(string, []driver.NamedValue) fakeResult {
				return fakeResult{cols: []string{"exists", "role"}, rows: [][]driver.Value{{c.orgExists, c.role}}}
			})
			role, member, err := b.OrgMemberRole(context.Background(), "o1", "u1")
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if role != c.wantRole || member != c.wantMember {
				t.Errorf("= (%q, %v), want (%q, %v)", role, member, c.wantRole, c.wantMember)
			}
			qs := f.Queries()
			if len(qs) != 1 {
				t.Fatalf("expected one statement, got %d", len(qs))
			}
			q := oneLine(qs[0].query)
			if !strings.Contains(q, "FROM org_members WHERE org_id = $1 AND user_id = $2") {
				t.Errorf("not a point lookup on (org_id, user_id): %s", q)
			}
			if len(qs[0].args) != 2 || qs[0].args[0].Value != "o1" || qs[0].args[1].Value != "u1" {
				t.Errorf("args = %v", qs[0].args)
			}
		})
	}
}

// TestListOrgMembershipsForUser_NoRoster: the library's org scope needs only
// the caller's org ids; ListOrgsForUser also fetched every member of every
// org (a second, roster-sized query).
func TestListOrgMembershipsForUser_NoRoster(t *testing.T) {
	b, f := newFakeBackend(t, 1, func(string, []driver.NamedValue) fakeResult {
		return fakeResult{
			cols: []string{"id", "name", "role"},
			rows: [][]driver.Value{{"o1", "One", "admin"}, {"o2", "Two", "viewer"}},
		}
	})
	ms, err := b.ListOrgMembershipsForUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ListOrgMembershipsForUser: %v", err)
	}
	want := []interfaces.OrgMembership{{OrgID: "o1", OrgName: "One", Role: auth.RoleAdmin}, {OrgID: "o2", OrgName: "Two", Role: auth.RoleViewer}}
	if len(ms) != len(want) || ms[0] != want[0] || ms[1] != want[1] {
		t.Errorf("= %+v, want %+v", ms, want)
	}
	qs := f.Queries()
	if len(qs) != 1 {
		t.Fatalf("expected one statement (no roster query), got %d", len(qs))
	}
	if q := oneLine(qs[0].query); !strings.Contains(q, "WHERE m.user_id = $1") || strings.Contains(q, "ANY(") {
		t.Errorf("unexpected membership query: %s", q)
	}
}

// TestGetAPITokenWithOwnerByHash_OneJoinedQuery: PAT authentication read the
// token, then the user, sequentially, on every machine request.
func TestGetAPITokenWithOwnerByHash_OneJoinedQuery(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	b, f := newFakeBackend(t, 1, func(string, []driver.NamedValue) fakeResult {
		return fakeResult{
			cols: make([]string, 17),
			rows: [][]driver.Value{{
				"t1", "u1", "ci", "hash", now, nil, "analyze,read",
				"u1", "a@example.test", "member", true, int64(0), nil,
				"Ann", "", now, now,
			}},
		}
	})
	tok, user, err := b.GetAPITokenWithOwnerByHash(context.Background(), "hash")
	if err != nil {
		t.Fatalf("GetAPITokenWithOwnerByHash: %v", err)
	}
	if tok.ID != "t1" || tok.UserID != "u1" || len(tok.Scopes) != 2 || tok.ExpiresAt != nil {
		t.Errorf("token = %+v", tok)
	}
	if user.ID != "u1" || user.Email != "a@example.test" || user.Role != auth.RoleMember || user.DisplayName != "Ann" {
		t.Errorf("user = %+v", user)
	}
	qs := f.Queries()
	if len(qs) != 1 {
		t.Fatalf("expected one round trip, got %d statements", len(qs))
	}
	if q := oneLine(qs[0].query); !strings.Contains(q, "JOIN users u ON u.id = t.user_id") || !strings.Contains(q, "WHERE t.token_hash = $1") {
		t.Errorf("not the joined token+owner lookup: %s", q)
	}

	b2, _ := newFakeBackend(t, 1, nil)
	if _, _, err := b2.GetAPITokenWithOwnerByHash(context.Background(), "nope"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("unknown token: err = %v, want ErrNotFound", err)
	}
}

// TestGetFindingStatus_SingleRow: the triage write needs the prior assignee
// of ONE finding; it listed every status of the flow.
func TestGetFindingStatus_SingleRow(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	b, f := newFakeBackend(t, 1, func(string, []driver.NamedValue) fakeResult {
		return fakeResult{
			cols: make([]string, 8),
			rows: [][]driver.Value{{"f1", "k1", "r1", "open", "", "bob", "alice", now}},
		}
	})
	st, err := b.GetFindingStatus(context.Background(), "f1", "k1")
	if err != nil || st == nil || st.AssigneeID != "bob" {
		t.Fatalf("GetFindingStatus = %+v, %v", st, err)
	}
	if q := oneLine(f.Queries()[0].query); !strings.Contains(q, "WHERE flow_id = $1 AND finding_key = $2") {
		t.Errorf("not a primary-key read: %s", q)
	}

	b2, _ := newFakeBackend(t, 1, nil)
	if st, err := b2.GetFindingStatus(context.Background(), "f1", "k1"); err != nil || st != nil {
		t.Errorf("no row: = %+v, %v; want nil, nil", st, err)
	}
}

// TestFlowFilterWhere_AfterIDKeyset: ScopeFlowIDs pages by id with a keyset
// cursor instead of OFFSET.
func TestFlowFilterWhere_AfterIDKeyset(t *testing.T) {
	where, args := flowFilterWhere(interfaces.FlowFilter{UserID: "u1", AfterID: "f-500", SortBy: interfaces.FlowSortIDAsc})
	if !strings.Contains(where, "id > $2") {
		t.Errorf("AfterID not applied as id > $2: %s", where)
	}
	if len(args) != 2 || args[1] != "f-500" {
		t.Errorf("args = %v", args)
	}
	if next := args.add("limit"); next != "$3" {
		t.Errorf("next placeholder = %s, want $3", next)
	}
	where, _ = flowFilterWhere(interfaces.FlowFilter{UserID: "u1"})
	if strings.Contains(where, "id >") {
		t.Errorf("no cursor must add no id predicate: %s", where)
	}
}

// TestPoolLimits_IdleMatchesOpen: 25 open / 5 idle closed every connection
// above five the moment it was returned and re-dialled it on the next burst.
func TestPoolLimits_IdleMatchesOpen(t *testing.T) {
	cases := []struct {
		name               string
		cfg                Config
		wantOpen, wantIdle int
	}{
		{"default", DefaultConfig("postgres://x"), 25, 25},
		{"open raised by env, idle unset", Config{MaxOpenConns: 60}, 60, 60},
		{"idle set explicitly by env", Config{MaxOpenConns: 60, MaxIdleConns: 10}, 60, 10},
		{"unlimited open, idle unset", Config{}, 0, 0},
	}
	for _, c := range cases {
		open, idle := poolLimits(c.cfg)
		if open != c.wantOpen || idle != c.wantIdle {
			t.Errorf("%s: poolLimits = (%d, %d), want (%d, %d)", c.name, open, idle, c.wantOpen, c.wantIdle)
		}
	}
}

// TestMigration26_PerfIndexesShape pins the v26 step's text: idempotent index
// creation (no CONCURRENTLY — migrations run in a transaction) and a down
// step that restores exactly what it dropped. Whether Postgres uses the
// indexes is NOT shown here; that needs a real server (EXPLAIN).
func TestMigration26_PerfIndexesShape(t *testing.T) {
	m, ok := migrationByVersion(26)
	if !ok {
		t.Fatal("migration v26 not registered")
	}
	if m.name != "perf_indexes_v26" {
		t.Errorf("v26 name = %q", m.name)
	}
	sqlText := oneLine(m.sql)
	for _, want := range []string{
		"CREATE INDEX IF NOT EXISTS flow_collaborators_user_flow_idx ON flow_collaborators (user_id, flow_id);",
		"CREATE INDEX IF NOT EXISTS gov_alerts_flow_idx ON gov_alerts (flow_id);",
		"CREATE INDEX IF NOT EXISTS gov_alerts_unread_idx ON gov_alerts (flow_id, target_user_id) WHERE read_at IS NULL AND dismissed_at IS NULL;",
		"CREATE INDEX IF NOT EXISTS usage_metrics_user_created_cost_idx ON usage_metrics (user_id, created_at) INCLUDE (estimated_cost);",
		"CREATE INDEX IF NOT EXISTS usage_metrics_org_created_cost_idx ON usage_metrics (org_id, created_at) INCLUDE (estimated_cost);",
		"DROP INDEX IF EXISTS usage_metrics_user_created_idx;",
		"DROP INDEX IF EXISTS usage_metrics_org_created_idx;",
		"DROP INDEX IF EXISTS flows_tags_idx;",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("v26 lacks %q", want)
		}
	}
	if strings.Contains(strings.ToUpper(m.sql), "CONCURRENTLY") {
		t.Error("v26 must not use CONCURRENTLY: applyMigration runs each step in a transaction")
	}
	if strings.Count(strings.ToUpper(m.sql), "CREATE INDEX") != strings.Count(strings.ToUpper(m.sql), "CREATE INDEX IF NOT EXISTS") {
		t.Error("every v26 CREATE INDEX must be IF NOT EXISTS")
	}
	down := oneLine(m.downSQL)
	for _, want := range []string{
		"DROP INDEX IF EXISTS flow_collaborators_user_flow_idx;",
		"DROP INDEX IF EXISTS gov_alerts_flow_idx;",
		"DROP INDEX IF EXISTS gov_alerts_unread_idx;",
		"CREATE INDEX IF NOT EXISTS usage_metrics_user_created_idx ON usage_metrics (user_id, created_at);",
		"CREATE INDEX IF NOT EXISTS usage_metrics_org_created_idx ON usage_metrics (org_id, created_at);",
		"DROP INDEX IF EXISTS usage_metrics_user_created_cost_idx;",
		"DROP INDEX IF EXISTS usage_metrics_org_created_cost_idx;",
		"CREATE INDEX IF NOT EXISTS flows_tags_idx ON flows (tags);",
	} {
		if !strings.Contains(down, want) {
			t.Errorf("v26 down lacks %q", want)
		}
	}
}

// TestFlowHeaderMetadataUnmarshal guards the header's metadata scan against
// the JSON shape SaveFlow writes (Go field names, see FlowMetadata).
func TestFlowHeaderMetadataUnmarshal(t *testing.T) {
	meta, err := json.Marshal(interfaces.FlowMetadata{BlockCount: 3, SubflowCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newFakeBackend(t, 1, func(string, []driver.NamedValue) fakeResult {
		now := time.Now()
		return fakeResult{cols: make([]string, 10), rows: [][]driver.Value{{"f1", "n", "", meta, "u1", "", now, now, int64(0), ""}}}
	})
	hdr, err := b.LoadFlowHeader(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Metadata.BlockCount != 3 || hdr.Metadata.SubflowCount != 2 || hdr.Tags != nil {
		t.Errorf("header = %+v", hdr)
	}
}
