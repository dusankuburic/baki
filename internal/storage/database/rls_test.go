package database_test

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

func cleanupUser(t *testing.T, b *database.PostgresStorageBackend, userID string) {
	t.Helper()
	_, _ = b.DB().ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
}

// skipIfRLSBypassed skips negative RLS assertions when the test connects as a
// role that bypasses Row-Level Security (a superuser or a BYPASSRLS role).
// Postgres ignores RLS for such roles even when FORCE is set, so isolation
// cannot be observed — the application must run as an unprivileged role for RLS
// to take effect. Positive paths still validate; only the "cannot see" checks
// are meaningless here.
func skipIfRLSBypassed(t *testing.T, b *database.PostgresStorageBackend) {
	t.Helper()
	var bypass bool
	err := b.DB().QueryRowContext(context.Background(),
		`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&bypass)
	if err != nil {
		t.Fatalf("check rls bypass: %v", err)
	}
	if bypass {
		t.Skip("connected as a superuser/BYPASSRLS role; RLS isolation cannot be validated — run as an unprivileged role")
	}
}

func TestPostgres_RLS_FlowIsolation(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	userA := "rls-user-a-" + time.Now().Format("150405.000000000")
	userB := "rls-user-b-" + time.Now().Format("150405.000000000")
	flowA := "rls-flow-a-" + time.Now().Format("150405.000000000")
	flowB := "rls-flow-b-" + time.Now().Format("150405.000000000")

	for _, id := range []string{userA, userB} {
		if err := b.CreateUser(ctx, &interfaces.User{
			ID:        id,
			Email:     id + "@test.com",
			Password:  "$2a$12$testhash",
			Role:      "member",
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range []string{flowA, flowB} {
			b.DeleteFlow(ctx, id)
		}
		for _, id := range []string{userA, userB} {
			cleanupUser(t, b, id)
		}
	})

	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
		ID: flowA, Name: "Flow A", OwnerID: userA, Content: []byte("{}"),
	}); err != nil {
		t.Fatalf("SaveFlow A: %v", err)
	}
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
		ID: flowB, Name: "Flow B", OwnerID: userB, Content: []byte("{}"),
	}); err != nil {
		t.Fatalf("SaveFlow B: %v", err)
	}

	t.Run("user A sees only own flows", func(t *testing.T) {
		tx, err := b.BeginRLS(ctx, userA)
		if err != nil {
			t.Fatalf("BeginRLS: %v", err)
		}
		defer tx.Commit()

		rlsCtx := database.WithRLSTx(ctx, tx)
		flows, err := b.ListFlows(rlsCtx, interfaces.FlowFilter{UserID: userA, Limit: 100})
		if err != nil {
			t.Fatalf("ListFlows: %v", err)
		}
		for _, f := range flows {
			if f.OwnerID != userA {
				t.Errorf("RLS leak: user %s can see flow owned by %s (id=%s)", userA, f.OwnerID, f.ID)
			}
		}
		foundA := false
		for _, f := range flows {
			if f.ID == flowA {
				foundA = true
			}
		}
		if !foundA {
			t.Error("RLS over-filtered: user A cannot see own flow")
		}
	})

	t.Run("user B sees only own flows", func(t *testing.T) {
		tx, err := b.BeginRLS(ctx, userB)
		if err != nil {
			t.Fatalf("BeginRLS: %v", err)
		}
		defer tx.Commit()

		rlsCtx := database.WithRLSTx(ctx, tx)
		flows, err := b.ListFlows(rlsCtx, interfaces.FlowFilter{UserID: userB, Limit: 100})
		if err != nil {
			t.Fatalf("ListFlows: %v", err)
		}
		for _, f := range flows {
			if f.OwnerID != userB {
				t.Errorf("RLS leak: user %s can see flow owned by %s (id=%s)", userB, f.OwnerID, f.ID)
			}
		}
	})

	t.Run("no RLS context sees all flows", func(t *testing.T) {
		// Load by ID (not an empty-filter ListFlows, which the flowFilterWhere
		// "1=0" guard would reject regardless of RLS): without an RLS context the
		// pool connection should be able to read either owner's flow.
		for _, id := range []string{flowA, flowB} {
			if _, err := b.LoadFlow(ctx, id); err != nil {
				t.Errorf("without RLS context flow %s should be visible: %v", id, err)
			}
		}
	})
}

func TestPostgres_RLS_CollaboratorAccess(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	owner := "rls-owner-" + time.Now().Format("150405.000000000")
	collab := "rls-collab-" + time.Now().Format("150405.000000000")
	flowID := "rls-collab-flow-" + time.Now().Format("150405.000000000")

	for _, id := range []string{owner, collab} {
		if err := b.CreateUser(ctx, &interfaces.User{
			ID:        id,
			Email:     id + "@test.com",
			Password:  "$2a$12$testhash",
			Role:      "member",
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		b.DeleteFlow(ctx, flowID)
		for _, id := range []string{owner, collab} {
			cleanupUser(t, b, id)
		}
	})

	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
		ID: flowID, Name: "Shared Flow", OwnerID: owner, Content: []byte("{}"),
	}); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	if err := b.AddCollaborator(ctx, flowID, &interfaces.Collaborator{
		UserID: collab, Permission: "viewer",
	}); err != nil {
		t.Fatalf("AddCollaborator: %v", err)
	}

	t.Run("collaborator can see shared flow via RLS", func(t *testing.T) {
		tx, err := b.BeginRLS(ctx, collab)
		if err != nil {
			t.Fatalf("BeginRLS: %v", err)
		}
		defer tx.Commit()

		rlsCtx := database.WithRLSTx(ctx, tx)
		flows, err := b.ListFlows(rlsCtx, interfaces.FlowFilter{UserID: collab, Limit: 100})
		if err != nil {
			t.Fatalf("ListFlows: %v", err)
		}
		found := false
		for _, f := range flows {
			if f.ID == flowID {
				found = true
			}
		}
		if !found {
			t.Error("RLS blocked collaborator from seeing shared flow")
		}
	})
}

func TestPostgres_RLS_OrgMemberAccess(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	orgOwner := "rls-org-owner-" + time.Now().Format("150405.000000000")
	orgMember := "rls-org-member-" + time.Now().Format("150405.000000000")
	orgID := "rls-org-" + time.Now().Format("150405.000000000")
	flowID := "rls-org-flow-" + time.Now().Format("150405.000000000")

	for _, id := range []string{orgOwner, orgMember} {
		if err := b.CreateUser(ctx, &interfaces.User{
			ID:        id,
			Email:     id + "@test.com",
			Password:  "$2a$12$testhash",
			Role:      "member",
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		b.DeleteFlow(ctx, flowID)
		b.DeleteOrg(ctx, orgID)
		for _, id := range []string{orgOwner, orgMember} {
			cleanupUser(t, b, id)
		}
	})

	if err := b.SaveOrg(ctx, &interfaces.Organisation{
		ID:      orgID,
		Name:    "Test Org",
		OwnerID: orgOwner,
		Members: []interfaces.OrgMember{
			{UserID: orgOwner, Role: "admin"},
			{UserID: orgMember, Role: "member"},
		},
	}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
		ID: flowID, Name: "Org Flow", OwnerID: orgOwner, OrganizationID: orgID, Content: []byte("{}"),
	}); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}

	t.Run("org member can see org flow via RLS", func(t *testing.T) {
		tx, err := b.BeginRLS(ctx, orgMember)
		if err != nil {
			t.Fatalf("BeginRLS: %v", err)
		}
		defer tx.Commit()

		rlsCtx := database.WithRLSTx(ctx, tx)
		// The real org library view scopes by org id; pass it so the app-level
		// filter includes org flows (membership itself is enforced upstream).
		flows, err := b.ListFlows(rlsCtx, interfaces.FlowFilter{UserID: orgMember, OrganizationID: orgID, Limit: 100})
		if err != nil {
			t.Fatalf("ListFlows: %v", err)
		}
		found := false
		for _, f := range flows {
			if f.ID == flowID {
				found = true
			}
		}
		if !found {
			t.Error("org member cannot see org flow")
		}
	})

	t.Run("non-member cannot see org flow via RLS", func(t *testing.T) {
		skipIfRLSBypassed(t, b)
		outsider := "rls-outsider-" + time.Now().Format("150405.000000000")
		if err := b.CreateUser(ctx, &interfaces.User{
			ID: outsider, Email: outsider + "@test.com", Password: "$2a$12$testhash",
			Role: "member", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateUser outsider: %v", err)
		}
		t.Cleanup(func() { cleanupUser(t, b, outsider) })

		tx, err := b.BeginRLS(ctx, outsider)
		if err != nil {
			t.Fatalf("BeginRLS: %v", err)
		}
		defer tx.Commit()

		rlsCtx := database.WithRLSTx(ctx, tx)
		_, err = b.LoadFlow(rlsCtx, flowID)
		if err == nil {
			t.Error("RLS leak: outsider can load org flow")
		}
	})
}

// TestPostgres_RLS_SaveKnowledgeChunks_EnforcesOrgMembership guards H1:
// SaveKnowledgeChunks MUST run inside an RLS-scoped tx so the
// knowledge_chunks WITH CHECK policy enforces org membership. Before the fix
// the method used a bare b.db.BeginTx with no GUC set, so the policy
// short-circuited to "allow" and any authenticated user could write chunks for
// an arbitrary org_id. Now a non-member's SaveKnowledgeChunks must fail at
// the RLS check.
func TestPostgres_RLS_SaveKnowledgeChunks_EnforcesOrgMembership(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	orgOwner := "rls-kn-owner-" + time.Now().Format("150405.000000000")
	outsider := "rls-kn-outsider-" + time.Now().Format("150405.000000000")
	orgID := "rls-kn-org-" + time.Now().Format("150405.000000000")
	docID := "rls-kn-doc-" + time.Now().Format("150405.000000000")

	for _, id := range []string{orgOwner, outsider} {
		if err := b.CreateUser(ctx, &interfaces.User{
			ID:        id,
			Email:     id + "@test.com",
			Password:  "$2a$12$testhash",
			Role:      "member",
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
	}
	if err := b.SaveOrg(ctx, &interfaces.Organisation{
		ID: orgID, Name: "KN Org", OwnerID: orgOwner,
		Members: []interfaces.OrgMember{{UserID: orgOwner, Role: "admin"}},
	}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	if err := b.SaveKnowledgeDocument(ctx, &interfaces.KnowledgeDocument{
		ID: docID, OrgID: orgID, Filename: "doc.txt", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveKnowledgeDocument: %v", err)
	}

	t.Cleanup(func() {
		_, _ = b.DB().ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id = $1`, docID)
		_, _ = b.DB().ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, docID)
		b.DeleteOrg(ctx, orgID)
		for _, id := range []string{orgOwner, outsider} {
			cleanupUser(t, b, id)
		}
	})

	// Sanity: owner can insert chunks into their own org's doc.
	ownerChunks := []interfaces.KnowledgeChunk{
		{ID: "chunk-owner-1", DocID: docID, Content: "ok", Embedding: []float32{0.1, 0.2}},
	}
	if err := b.SaveKnowledgeChunks(ctx, orgOwner, ownerChunks); err != nil {
		t.Fatalf("owner SaveKnowledgeChunks should succeed: %v", err)
	}

	// H1 regression guard: a non-member writing a chunk whose doc belongs to
	// an org they don't belong to must be rejected by the RLS WITH CHECK
	// policy (silent success here = the H1 bug has returned).
	skipIfRLSBypassed(t, b)
	outsiderChunks := []interfaces.KnowledgeChunk{
		{ID: "chunk-outsider-1", DocID: docID, Content: "leaked", Embedding: []float32{0.3, 0.4}},
	}
	if err := b.SaveKnowledgeChunks(ctx, outsider, outsiderChunks); err == nil {
		t.Error("RLS leak: outsider's SaveKnowledgeChunks succeeded — the H1 fix has regressed")
	}
}

// TestPostgres_RLS_AllTablesForced is the regression guard for migration v19.
//
// `ENABLE ROW LEVEL SECURITY` exempts the table OWNER from its own policies;
// only `FORCE` closes that. Because this deployment runs its migrations at
// boot, the application role owns every table it queries — so a table with
// ENABLE-but-no-FORCE has policies that are inert in production while still
// looking protected in the schema. Ten tables were in that state.
//
// Unlike the isolation subtests below, this one is a catalog query, so it runs
// even when connected as a superuser (which is how CI connects). That is
// deliberate: it is the only RLS assertion in this file that is guaranteed to
// execute in CI.
func TestPostgres_RLS_AllTablesForced(t *testing.T) {
	b := openTestDB(t)

	rows, err := b.DB().QueryContext(context.Background(), `
		SELECT c.relname
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = current_schema()
		   AND c.relrowsecurity
		   AND NOT c.relforcerowsecurity
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("query pg_class: %v", err)
	}
	defer rows.Close()

	var unforced []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		unforced = append(unforced, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(unforced) > 0 {
		t.Errorf("tables have RLS enabled but not FORCED (their policies do not apply to the owning app role): %v\n"+
			"add `ALTER TABLE <name> FORCE ROW LEVEL SECURITY;` in a new migration", unforced)
	}

	// Sanity: the query must actually be looking at RLS tables, otherwise an
	// empty result would pass vacuously (e.g. wrong schema, migrations not run).
	var enabled int
	if err := b.DB().QueryRowContext(context.Background(), `
		SELECT count(*) FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = current_schema() AND c.relrowsecurity`).Scan(&enabled); err != nil {
		t.Fatalf("count rls tables: %v", err)
	}
	if enabled < 12 {
		t.Errorf("expected >=12 RLS-enabled tables, found %d — migrations may not have run", enabled)
	}
}
