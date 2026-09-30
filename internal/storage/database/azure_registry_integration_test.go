package database_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

// azureTestUser creates a user row (the registry's foreign key needs one),
// removed on cleanup — its registry rows cascade.
func azureTestUser(t *testing.T, b *database.PostgresStorageBackend, prefix string) string {
	t.Helper()
	id := fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), contract.NewAzureTestID())
	if err := b.CreateUser(context.Background(), &interfaces.User{
		ID: id, Email: id + "@example.com", Password: "$2a$12$testhash", Role: "member",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, b, id) })
	return id
}

// azureTestOrg creates an organisation whose members are given, removed on
// cleanup (its registry rows cascade).
func azureTestOrg(t *testing.T, b *database.PostgresStorageBackend, members ...interfaces.OrgMember) string {
	t.Helper()
	id := fmt.Sprintf("azure-org-%d-%s", time.Now().UnixNano(), contract.NewAzureTestID())
	owner := "nobody"
	if len(members) > 0 {
		owner = members[0].UserID
	}
	if err := b.SaveOrg(context.Background(), &interfaces.Organisation{ID: id, Name: "Azure " + id, OwnerID: owner, Members: members}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), id) })
	return id
}

func azureOwnerFor(t *testing.T, b *database.PostgresStorageBackend, kind interfaces.AzureOwnerKind) interfaces.AzureOwner {
	t.Helper()
	if kind == interfaces.AzureOwnerOrg {
		return interfaces.AzureOrgOwner(azureTestOrg(t, b))
	}
	return interfaces.AzureUserOwner(azureTestUser(t, b, "azure-user"))
}

// TestPostgresAzureRegistry_Contract runs the shared registry contract on
// Postgres, for users and organisations, without an RLS transaction — the
// /api/chat/* shape, where only the queries' owner predicates separate the
// owners (the isolation scenario).
func TestPostgresAzureRegistry_Contract(t *testing.T) {
	b := openTestDB(t)
	store := database.NewPostgresAzureRegistry(b)
	contract.RunAzureRegistrySuite(t, contract.AzureRegistryHarness{
		Fresh: func(t *testing.T, kind interfaces.AzureOwnerKind) (interfaces.AzureRegistryStore, interfaces.AzureOwner, bool) {
			return store, azureOwnerFor(t, b, kind), true
		},
		Another: func(t *testing.T, _ interfaces.AzureRegistryStore, kind interfaces.AzureOwnerKind) (interfaces.AzureOwner, bool) {
			return azureOwnerFor(t, b, kind), true
		},
	})
}

func pgConn(id, name string) interfaces.AzureConnectionRecord {
	return interfaces.AzureConnectionRecord{ID: id, Name: name}
}

func pgModel(id, conn, kind, deployment string) interfaces.AzureModelRecord {
	return interfaces.AzureModelRecord{ID: id, ConnectionID: conn, Kind: kind, Deployment: deployment,
		DisplayName: deployment, Purpose: interfaces.AzurePurposeGeneral}
}

// TestPostgresAzureRegistry_CascadesInsideBeginRLS: inside a request's RLS
// transaction the registry's writes, the resource→models cascade included,
// commit or roll back with the request; the owner row's deletion cascades
// the whole registry (account erasure, organisation deletion).
func TestPostgresAzureRegistry_CascadesInsideBeginRLS(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	store := database.NewPostgresAzureRegistry(b)
	uid := azureTestUser(t, b, "azure-rls")
	owner := interfaces.AzureUserOwner(uid)

	// A request that fails: everything it wrote goes.
	tx, err := b.BeginRLS(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	rctx := database.WithRLSTx(ctx, tx)
	if _, err := store.CreateConnection(rctx, owner, pgConn("default", "res")); err != nil {
		t.Fatal(err)
	}
	for _, m := range []interfaces.AzureModelRecord{
		pgModel("aaaaaaaaaa", "default", interfaces.AzureKindChat, "gpt-4.1"),
		pgModel("bbbbbbbbbb", "default", interfaces.AzureKindEmbedding, "text-embedding-3-large"),
	} {
		if _, err := store.CreateModel(rctx, owner, m); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := store.DeleteConnection(rctx, owner, "default")
	if err != nil || len(deleted) != 2 {
		t.Fatalf("cascade inside the transaction = (%d models, %v), want 2", len(deleted), err)
	}
	if models, _ := store.ListModels(rctx, owner); len(models) != 0 {
		t.Errorf("models visible after the cascade inside the transaction: %d", len(models))
	}
	if _, err := store.CreateConnection(rctx, owner, pgConn("cccccccccc", "doomed")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.Counts(ctx, owner); n != (interfaces.AzureRegistryCounts{}) {
		t.Errorf("a rolled-back request left rows: %+v", n)
	}

	// A request that succeeds: its rows are there after the commit.
	tx, err = b.BeginRLS(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	rctx = database.WithRLSTx(ctx, tx)
	if _, err := store.CreateConnection(rctx, owner, pgConn("default", "res")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(rctx, owner, pgModel("aaaaaaaaaa", "default", interfaces.AzureKindChat, "gpt-4.1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkImported(rctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.Counts(ctx, owner); n.Connections != 1 || n.Models != 1 {
		t.Fatalf("after the commit: %+v", n)
	}

	// Account erasure: the user row goes, the registry and its marker with it.
	if err := b.DeleteUser(ctx, uid); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	for _, table := range []string{"user_azure_connections", "user_azure_models", "user_azure_registry_state"} {
		var n int
		if err := b.DB().QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE user_id = $1`, uid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("erasure left %d rows in %s", n, table)
		}
	}

	// Organisation deletion cascades the org registry.
	orgID := azureTestOrg(t, b)
	org := interfaces.AzureOrgOwner(orgID)
	if _, err := store.CreateConnection(ctx, org, pgConn("default", "shared")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(ctx, org, pgModel("aaaaaaaaaa", "default", interfaces.AzureKindAgent, "review-bot")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkImported(ctx, org); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteOrg(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"org_azure_connections", "org_azure_models", "org_azure_registry_state"} {
		var n int
		if err := b.DB().QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE org_id = $1`, orgID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("organisation deletion left %d rows in %s", n, table)
		}
	}
}

// TestPostgresAzureRegistry_DuplicateKeepsTheRequestUsable: a 23505 inside
// the request transaction is mapped to its typed error under a savepoint,
// so the handler can still read (to name the existing model in its 409)
// and the request can still commit its other writes.
func TestPostgresAzureRegistry_DuplicateKeepsTheRequestUsable(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	store := database.NewPostgresAzureRegistry(b)
	uid := azureTestUser(t, b, "azure-dup")
	owner := interfaces.AzureUserOwner(uid)

	tx, err := b.BeginRLS(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rctx := database.WithRLSTx(ctx, tx)
	if _, err := store.CreateConnection(rctx, owner, pgConn("default", "Contoso")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(rctx, owner, pgModel("aaaaaaaaaa", "default", interfaces.AzureKindChat, "gpt-4.1")); err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateConnection(rctx, owner, pgConn("bbbbbbbbbb", "CONTOSO"))
	if !errors.Is(err, interfaces.ErrAzureDuplicateConnectionName) || !strings.Contains(err.Error(), "user_azure_connections_name_idx") {
		t.Fatalf("duplicate name = %v, want ErrAzureDuplicateConnectionName naming the index", err)
	}
	_, err = store.CreateModel(rctx, owner, pgModel("cccccccccc", "default", interfaces.AzureKindClaude, "GPT-4.1"))
	if !errors.Is(err, interfaces.ErrAzureDuplicateModel) || !strings.Contains(err.Error(), "user_azure_models_deployment_idx") {
		t.Fatalf("duplicate deployment = %v, want ErrAzureDuplicateModel naming the index", err)
	}
	// Still usable: a read, then another write, then the commit.
	if models, err := store.ListModels(rctx, owner); err != nil || len(models) != 1 {
		t.Fatalf("read after the duplicate = (%d, %v)", len(models), err)
	}
	if _, err := store.CreateConnection(rctx, owner, pgConn("dddddddddd", "Fabrikam")); err != nil {
		t.Fatalf("write after the duplicate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit after the duplicates: %v", err)
	}
	if n, _ := store.Counts(ctx, owner); n.Connections != 2 || n.Models != 1 {
		t.Errorf("committed %+v, want 2 resources and 1 model", n)
	}
}

// TestPostgresAzureRegistry_CompareAndSwapWithinOneTransaction: updated_at
// moves strictly forward even for two updates in one transaction (NOW()
// would hand out the same token twice), so a token read before the first
// update is stale after it.
func TestPostgresAzureRegistry_CompareAndSwapWithinOneTransaction(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	store := database.NewPostgresAzureRegistry(b)
	uid := azureTestUser(t, b, "azure-cas")
	owner := interfaces.AzureUserOwner(uid)
	c, err := store.CreateConnection(ctx, owner, pgConn("default", "cas"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := b.BeginRLS(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rctx := database.WithRLSTx(ctx, tx)
	u1, err := store.UpdateConnection(rctx, owner, c.ID, interfaces.AzureConnectionUpdate{Name: "one", ExpectedUpdatedAt: c.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	u2, err := store.UpdateConnection(rctx, owner, c.ID, interfaces.AzureConnectionUpdate{Name: "two", ExpectedUpdatedAt: u1.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	if !u2.UpdatedAt.After(u1.UpdatedAt) || !u1.UpdatedAt.After(c.UpdatedAt) {
		t.Errorf("tokens %v -> %v -> %v must strictly increase within one transaction", c.UpdatedAt, u1.UpdatedAt, u2.UpdatedAt)
	}
	_, err = store.UpdateConnection(rctx, owner, c.ID, interfaces.AzureConnectionUpdate{Name: "stale", ExpectedUpdatedAt: u1.UpdatedAt})
	if !errors.Is(err, interfaces.ErrAzureRegistryConflict) {
		t.Errorf("a token from before the second update = %v, want ErrAzureRegistryConflict", err)
	}
}

// TestPostgresAzureRegistry_OwnerLockTransaction: without a request
// transaction WithOwnerLock opens its own — fn's writes commit only when
// fn succeeds, and post-commit hooks (the keystore delete after a registry
// delete) run only after that commit, never on failure.
func TestPostgresAzureRegistry_OwnerLockTransaction(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	store := database.NewPostgresAzureRegistry(b)
	owner := interfaces.AzureUserOwner(azureTestUser(t, b, "azure-lock"))

	hookRan := false
	boom := errors.New("secret save failed")
	err := store.WithOwnerLock(ctx, owner, func(ctx context.Context) error {
		if _, err := store.CreateConnection(ctx, owner, pgConn("default", "res")); err != nil {
			return err
		}
		database.RegisterPostCommit(ctx, func() { hookRan = true })
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithOwnerLock = %v, want fn's error", err)
	}
	if hookRan {
		t.Error("a post-commit hook ran although fn failed")
	}
	if n, _ := store.Counts(ctx, owner); n.Connections != 0 {
		t.Errorf("fn failed but its write committed: %+v", n)
	}

	var ranAfterCommit bool
	err = store.WithOwnerLock(ctx, owner, func(ctx context.Context) error {
		if _, err := store.CreateConnection(ctx, owner, pgConn("default", "res")); err != nil {
			return err
		}
		database.RegisterPostCommit(ctx, func() {
			// Visible from another connection: the write had committed.
			n, err := store.Counts(context.Background(), owner)
			ranAfterCommit = err == nil && n.Connections == 1
		})
		if hookRan {
			t.Error("the hook ran before fn returned")
		}
		return nil
	})
	if err != nil || !ranAfterCommit {
		t.Errorf("WithOwnerLock = %v; hook saw the committed write: %v", err, ranAfterCommit)
	}

	// Inside a request transaction the lock joins it (and nests).
	uid := owner.ID
	tx, err := b.BeginRLS(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rctx := database.WithRLSTx(ctx, tx)
	err = store.WithOwnerLock(rctx, owner, func(ctx context.Context) error {
		return store.WithOwnerLock(ctx, owner, func(ctx context.Context) error {
			_, err := store.CreateConnection(ctx, owner, pgConn("bbbbbbbbbb", "in-request"))
			return err
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.Counts(ctx, owner); n.Connections != 1 {
		t.Errorf("a write under the lock inside a rolled-back request persisted: %+v", n)
	}
}

// TestPostgresAzureRegistry_ConcurrentImport: the lazy legacy import runs
// on read paths, so several requests race it. Under the owner lock the
// marker check makes it run once; and even without the lock the inserts
// (deterministic ids, ON CONFLICT DO NOTHING without a target) cannot
// duplicate a row or fail.
func TestPostgresAzureRegistry_ConcurrentImport(t *testing.T) {
	b := openTestDB(t)
	store := database.NewPostgresAzureRegistry(b)
	rows := []interfaces.AzureModelRecord{
		pgModel("chat000001", "default", interfaces.AzureKindChat, "gpt-4.1"),
		pgModel("claude0001", "default", interfaces.AzureKindClaude, "claude-sonnet-4-6"),
		pgModel("embed00001", "default", interfaces.AzureKindEmbedding, "text-embedding-3-large"),
	}
	rows[2].IsKnowledgeEmbedding = true
	importAll := func(ctx context.Context, owner interfaces.AzureOwner, useMarker bool) (int, error) {
		if useMarker {
			if _, ok, err := store.ImportedAt(ctx, owner); err != nil || ok {
				return 0, err
			}
		}
		created := 0
		if ok, err := store.ImportConnection(ctx, owner, pgConn("default", "my-res")); err != nil {
			return 0, err
		} else if ok {
			created++
		}
		for _, m := range rows {
			ok, err := store.ImportModel(ctx, owner, m)
			if err != nil {
				return created, err
			}
			if ok {
				created++
			}
		}
		_, err := store.MarkImported(ctx, owner)
		return created, err
	}
	for _, locked := range []bool{true, false} {
		t.Run(fmt.Sprintf("locked=%v", locked), func(t *testing.T) {
			owner := interfaces.AzureUserOwner(azureTestUser(t, b, "azure-import"))
			const racers = 8
			var (
				wg      sync.WaitGroup
				mu      sync.Mutex
				created int
				errs    []error
			)
			for range racers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var n int
					var err error
					if locked {
						err = store.WithOwnerLock(context.Background(), owner, func(ctx context.Context) error {
							var e error
							n, e = importAll(ctx, owner, true)
							return e
						})
					} else {
						n, err = importAll(context.Background(), owner, false)
					}
					mu.Lock()
					defer mu.Unlock()
					created += n
					if err != nil {
						errs = append(errs, err)
					}
				}()
			}
			wg.Wait()
			if len(errs) > 0 {
				t.Fatalf("import errors: %v", errs)
			}
			if created != 1+len(rows) {
				t.Errorf("rows created across the racers = %d, want %d (each exactly once)", created, 1+len(rows))
			}
			n, _ := store.Counts(context.Background(), owner)
			if n.Connections != 1 || n.Models != len(rows) {
				t.Errorf("registry after the race = %+v", n)
			}
			if _, ok, _ := store.ImportedAt(context.Background(), owner); !ok {
				t.Error("no import marker after the race")
			}
		})
	}
}

// TestPostgresAzureRegistry_RLSNegatives: with the request's RLS
// transaction active, the policies alone hide another user's registry and
// a non-member's view of an org's — even when the query's own owner
// predicate names that owner — and refuse writes into it. Needs an
// unprivileged role (a superuser bypasses RLS).
func TestPostgresAzureRegistry_RLSNegatives(t *testing.T) {
	b := openTestDB(t)
	skipIfRLSBypassed(t, b)
	ctx := context.Background()
	store := database.NewPostgresAzureRegistry(b)
	alice, bob := azureTestUser(t, b, "azure-alice"), azureTestUser(t, b, "azure-bob")
	outsider := azureTestUser(t, b, "azure-outsider")
	orgID := azureTestOrg(t, b, interfaces.OrgMember{UserID: alice, Role: "admin"}, interfaces.OrgMember{UserID: bob, Role: "member"})
	aliceOwner, org := interfaces.AzureUserOwner(alice), interfaces.AzureOrgOwner(orgID)
	for _, o := range []interfaces.AzureOwner{aliceOwner, org} {
		if _, err := store.CreateConnection(ctx, o, pgConn("default", "res")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateModel(ctx, o, pgModel("aaaaaaaaaa", "default", interfaces.AzureKindChat, "gpt-4.1")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.MarkImported(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	as := func(t *testing.T, userID string, fn func(ctx context.Context)) {
		t.Helper()
		tx, err := b.BeginRLS(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		fn(database.WithRLSTx(ctx, tx))
	}

	t.Run("another user sees nothing of a personal registry", func(t *testing.T) {
		as(t, bob, func(rctx context.Context) {
			if conns, err := store.ListConnections(rctx, aliceOwner); err != nil || len(conns) != 0 {
				t.Errorf("bob lists alice's resources: (%d, %v)", len(conns), err)
			}
			if _, err := store.GetModel(rctx, aliceOwner, "aaaaaaaaaa"); !errors.Is(err, interfaces.ErrAzureModelNotFound) {
				t.Errorf("bob reads alice's model: %v", err)
			}
			if _, ok, err := store.ImportedAt(rctx, aliceOwner); err != nil || ok {
				t.Errorf("bob reads alice's import marker: (%v, %v)", ok, err)
			}
			if _, err := store.DeleteModel(rctx, aliceOwner, "aaaaaaaaaa"); !errors.Is(err, interfaces.ErrAzureModelNotFound) {
				t.Errorf("bob deletes alice's model: %v", err)
			}
		})
		as(t, bob, func(rctx context.Context) {
			if _, err := store.CreateConnection(rctx, aliceOwner, pgConn("bbbbbbbbbb", "planted")); err == nil {
				t.Error("bob wrote a resource into alice's registry")
			}
		})
		if n, _ := store.Counts(ctx, aliceOwner); n.Connections != 1 || n.Models != 1 {
			t.Errorf("alice's registry after bob's attempts = %+v", n)
		}
	})

	t.Run("members see the org registry, outsiders do not", func(t *testing.T) {
		as(t, bob, func(rctx context.Context) {
			if models, err := store.ListModels(rctx, org); err != nil || len(models) != 1 {
				t.Errorf("a member lists the org's models: (%d, %v), want 1", len(models), err)
			}
		})
		as(t, outsider, func(rctx context.Context) {
			if conns, err := store.ListConnections(rctx, org); err != nil || len(conns) != 0 {
				t.Errorf("an outsider lists the org's resources: (%d, %v)", len(conns), err)
			}
			if _, ok, err := store.ImportedAt(rctx, org); err != nil || ok {
				t.Errorf("an outsider reads the org's import marker: (%v, %v)", ok, err)
			}
			if err := store.SetDesignation(rctx, org, interfaces.AzureDesignationOrgDefault, "aaaaaaaaaa"); !errors.Is(err, interfaces.ErrAzureModelNotFound) {
				t.Errorf("an outsider designates the org's model: %v", err)
			}
		})
		as(t, outsider, func(rctx context.Context) {
			if _, err := store.CreateConnection(rctx, org, pgConn("bbbbbbbbbb", "planted")); err == nil {
				t.Error("an outsider wrote a resource into the org's registry")
			}
		})
		if n, _ := store.Counts(ctx, org); n.Connections != 1 {
			t.Errorf("the org registry after the outsider's attempts = %+v", n)
		}
	})
}

// TestPostgresAzureRegistry_OwnerPredicatesWithoutRLS: on the pool (no RLS
// transaction — app_rls_active() is false, the policies allow everything)
// the queries' own owner predicates must keep owners apart: every method
// addressed to one owner, with another owner's ids, finds nothing, and a
// model can never hang off another owner's resource (the composite foreign
// key includes the owner).
func TestPostgresAzureRegistry_OwnerPredicatesWithoutRLS(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	var active bool
	if err := b.DB().QueryRowContext(ctx, `SELECT app_rls_active()`).Scan(&active); err != nil || active {
		t.Fatalf("app_rls_active() on the pool = (%v, %v), want false", active, err)
	}
	store := database.NewPostgresAzureRegistry(b)
	a := interfaces.AzureUserOwner(azureTestUser(t, b, "azure-pred-a"))
	other := interfaces.AzureUserOwner(azureTestUser(t, b, "azure-pred-b"))
	org := interfaces.AzureOrgOwner(azureTestOrg(t, b))
	if _, err := store.CreateConnection(ctx, a, pgConn("aconn00001", "a-res")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(ctx, a, pgModel("amodel0001", "aconn00001", interfaces.AzureKindEmbedding, "embed")); err != nil {
		t.Fatal(err)
	}
	for _, o := range []interfaces.AzureOwner{other, org} {
		if _, err := store.CreateConnection(ctx, o, pgConn("default", "own")); err != nil {
			t.Fatal(err)
		}
		checks := map[string]error{}
		_, checks["GetConnection"] = store.GetConnection(ctx, o, "aconn00001")
		_, checks["UpdateConnection"] = store.UpdateConnection(ctx, o, "aconn00001", interfaces.AzureConnectionUpdate{Name: "x"})
		_, checks["SetDiscoveryDismissed"] = store.SetDiscoveryDismissed(ctx, o, "aconn00001", true)
		checks["RecordConnectionTest"] = store.RecordConnectionTest(ctx, o, "aconn00001", &interfaces.AzureTestResult{OK: true})
		checks["FinalizeConnection"] = store.FinalizeConnection(ctx, o, "aconn00001")
		_, checks["DeleteConnection"] = store.DeleteConnection(ctx, o, "aconn00001")
		_, checks["GetModel"] = store.GetModel(ctx, o, "amodel0001")
		_, checks["UpdateModel"] = store.UpdateModel(ctx, o, "amodel0001", interfaces.AzureModelUpdate{
			ConnectionID: "default", Kind: interfaces.AzureKindEmbedding, Deployment: "x", DisplayName: "x", Purpose: interfaces.AzurePurposeGeneral})
		checks["RecordModelTest"] = store.RecordModelTest(ctx, o, "amodel0001", &interfaces.AzureTestResult{OK: true})
		checks["SetDesignation"] = store.SetDesignation(ctx, o, interfaces.AzureDesignationKnowledgeEmbedding, "amodel0001")
		_, checks["DeleteModel"] = store.DeleteModel(ctx, o, "amodel0001")
		_, checks["CreateModel on the other owner's resource"] = store.CreateModel(ctx, o, pgModel("omodel0001", "aconn00001", interfaces.AzureKindChat, "gpt"))
		_, checks["ImportModel on the other owner's resource"] = store.ImportModel(ctx, o, pgModel("omodel0002", "aconn00001", interfaces.AzureKindChat, "gpt"))
		for name, err := range checks {
			if !errors.Is(err, interfaces.ErrNotFound) {
				t.Errorf("%s as %s against the other owner's ids = %v, want not found", name, o, err)
			}
		}
		if models, _ := store.ListModels(ctx, o); len(models) != 0 {
			t.Errorf("%s lists %d models", o, len(models))
		}
	}
	got, err := store.GetConnection(ctx, a, "aconn00001")
	if err != nil || got.Name != "a-res" || got.LastTest != nil || got.DiscoveryDismissedAt != nil {
		t.Errorf("the owner's resource after the others' attempts = (%+v, %v), want untouched", got, err)
	}
	m, err := store.GetModel(ctx, a, "amodel0001")
	if err != nil || m.Deployment != "embed" || m.IsKnowledgeEmbedding || m.LastTest != nil {
		t.Errorf("the owner's model after the others' attempts = (%+v, %v), want untouched", m, err)
	}
}

// TestPostgresAzureRegistry_UnknownOwnerRow: a user id without a users row
// (the foreign key) is ErrAzureOwnerNotFound, and the empty id — the file
// store's desktop owner — is refused outright.
func TestPostgresAzureRegistry_UnknownOwnerRow(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	store := database.NewPostgresAzureRegistry(b)
	_, err := store.CreateConnection(ctx, interfaces.AzureUserOwner("no-such-user-"+contract.NewAzureTestID()), pgConn("default", "res"))
	if !errors.Is(err, interfaces.ErrAzureOwnerNotFound) {
		t.Errorf("a missing user = %v, want ErrAzureOwnerNotFound", err)
	}
	if _, err := store.ListConnections(ctx, interfaces.AzureUserOwner("")); !errors.Is(err, interfaces.ErrAzureRegistryOwnerUnsupported) {
		t.Errorf("the empty owner = %v, want ErrAzureRegistryOwnerUnsupported", err)
	}
	if store.PendingWrites() || store.BootID() != "" {
		t.Error("the database store must not use the pending protocol")
	}
	if _, ok := any(b).(database.AzureRegistryProvider); !ok {
		t.Error("the backend must expose its registry through AzureRegistryProvider")
	}
}
