package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// v48 runs while the release before still serves. These tests run its step, as the runner does (applyMigrationOnce:
// one attempt, so a lost deadlock shows), against that release's own statements in a second session, interleaved
// where the step waits for a lock. The step must neither deadlock with them (each side's 40P01 is a failed request or
// a step that is rolled back and redone) nor make a sign-in wait while it waits for a lock.

// seed48Concurrency rolls b back to v47 and stores what the scenarios touch: an organisation with an id-less policy,
// a channel naming two members, a knowledge document and an invitation; one to delete with its content; flows with
// share links (one without a creator, as the release before could leave them); accounts with sessions; a second
// organisation with a chunk of its own and one filed under it though its document is the first's, and a live session
// of an erased account (rows the release before can still reach).
func seed48Concurrency(t *testing.T, b *PostgresStorageBackend) {
	t.Helper()
	ctx := context.Background()
	if rolled, err := b.MigrateDown(ctx, 47); err != nil || len(rolled) == 0 || rolled[len(rolled)-1] != 48 {
		t.Fatalf("MigrateDown(47) = %v, %v", rolled, err)
	}
	for _, s := range []string{
		`INSERT INTO users (id, email, password, role) VALUES ('c48-owner', 'c48-owner@example.com', 'x', 'member'),
			('c48-erased', 'c48-erased@example.com', 'x', 'member'), ('c48-member', 'c48-member@example.com', 'x', 'member'),
			('c48-u4', 'c48-u4@example.com', 'x', 'member'), ('c48-invitee', 'c48-invitee@example.com', 'x', 'member')`,
		`INSERT INTO organisations (id, name, owner_id) VALUES ('c48-org', 'Org', 'c48-owner'), ('c48-gone', 'Gone', 'c48-owner'),
			('c48-other', 'Other', 'c48-owner')`,
		`INSERT INTO org_members (org_id, user_id, role) VALUES ('c48-org', 'c48-owner', 'admin'), ('c48-org', 'c48-erased', 'member'),
			('c48-org', 'c48-u4', 'member'), ('c48-gone', 'c48-owner', 'admin'), ('c48-other', 'c48-owner', 'admin')`,
		`INSERT INTO policies (id, org_id, name, description) VALUES ('', 'c48-org', 'Id-less', ''), ('', 'c48-gone', 'Id-less', ''),
			('c48-pol', 'c48-org', 'Kept', '')`,
		`INSERT INTO org_channels (id, org_id, kind, url, config) VALUES
			('c48-ch', 'c48-org', 'email', '', '{"recipients": "c48-erased,c48-u4,c48-member"}'), ('c48-ch-gone', 'c48-gone', 'webhook', 'u', '{}'),
			('c48-ch-orphan', 'c48-no-such-org', 'webhook', 'u', '{}')`,
		`INSERT INTO org_custom_rules (id, org_id, rule_id, config) VALUES ('c48-cr', 'c48-org', 'r', '{}'), ('c48-cr-gone', 'c48-gone', 'r', '{}')`,
		`INSERT INTO knowledge_documents (id, org_id, filename) VALUES ('c48-kd', 'c48-org', 'a.md'), ('c48-kd-gone', 'c48-gone', 'b.md'),
			('c48-kd-other', 'c48-other', 'c.md')`,
		`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding) VALUES ('c48-kc', 'c48-kd', 'c48-org', 'x', '[]'),
			('c48-kc-other', 'c48-kd-other', 'c48-other', 'x', '[]'), ('c48-kc-wrong', 'c48-kd', 'c48-other', 'x', '[]')`,
		`INSERT INTO org_invites (id, org_id, email, role, invited_by, token_hash, expires_at) VALUES
			('c48-inv', 'c48-org', 'c48-invitee@example.com', 'member', 'c48-erased', 'c48-hash', NOW() + INTERVAL '1 day')`,
		`INSERT INTO flows (id, name, owner_id) VALUES ('c48-f10', 'F10', 'c48-owner'), ('c48-f3', 'F3', 'c48-erased')`,
		`INSERT INTO share_tokens (id, flow_id, token_hash, created_by) VALUES ('c48-s10', 'c48-f10', 'c48-s10-hash', NULL),
			('c48-s3', 'c48-f3', 'c48-s3-hash', 'c48-erased'), ('c48-s11', 'c48-f10', 'c48-s11-hash', 'c48-u4')`,
		`INSERT INTO refresh_tokens (jti, user_id, expires_at) VALUES ('c48-rt-erased', 'c48-erased', NOW() + INTERVAL '1 hour'),
			('c48-rt-u4', 'c48-u4', NOW() + INTERVAL '1 hour'), ('c48-rt-orphan', 'c48-no-such-user', NOW() + INTERVAL '1 hour')`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
}

// step48 is one attempt of the v48 step on a connection of its own, started in the background.
type step48 struct {
	pid  int
	done chan error
}

func startStep48(t *testing.T, b *PostgresStorageBackend) *step48 {
	t.Helper()
	ctx := context.Background()
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := liftSessionTimeouts(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	s := &step48{done: make(chan error, 1)}
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&s.pid); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer conn.Close()
		defer restore()
		s.done <- applyMigrationOnce(ctx, conn, mustMigrationByVersion(48))
	}()
	return s
}

// wait returns the step's outcome, failing the test when it runs on past the budget.
func (s *step48) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-s.done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the step did not end within 30 s")
		return nil
	}
}

// waitForTheStep returns once the step's backend waits: for a lock (a table's or a row's), or in a pause between
// its tries for one.
func waitForTheStep(t *testing.T, b *PostgresStorageBackend, s *step48) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var kind, event sql.NullString
		if err := b.db.QueryRowContext(context.Background(),
			`SELECT wait_event_type, wait_event FROM pg_stat_activity WHERE pid = $1`, s.pid).Scan(&kind, &event); err != nil {
			t.Fatal(err)
		}
		if kind.String == "Lock" || event.String == "PgSleep" {
			return
		}
		select {
		case err := <-s.done:
			t.Fatalf("the step ended (%v) before it waited for anything; the scenario's blocker did not block it", err)
		case <-time.After(2 * time.Millisecond):
		}
	}
	t.Fatal("the step never waited for a lock")
}

// session is a transaction of the release before, run statement by statement.
type session struct {
	t    *testing.T
	conn *sql.Conn
	tx   *sql.Tx
}

func openSession(t *testing.T, b *PostgresStorageBackend) *session {
	t.Helper()
	ctx := context.Background()
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(); _ = conn.Close() })
	return &session{t: t, conn: conn, tx: tx}
}

// placeholders is the highest $n a statement names.
func placeholders(q string) int {
	n := 0
	for i := 1; i <= 9; i++ {
		if strings.Contains(q, fmt.Sprintf("$%d", i)) {
			n = i
		}
	}
	return n
}

// run executes the statements in order and commits; the first error is returned, naming its statement.
func (s *session) run(stmts []string, args ...any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, q := range stmts {
		if _, err := s.tx.ExecContext(ctx, q, args[:placeholders(q)]...); err != nil {
			return fmt.Errorf("%s: %w", oneLine(q), err)
		}
	}
	return s.tx.Commit()
}

// must runs the statements without committing, failing the test on an error.
func (s *session) must(stmts []string, args ...any) {
	s.t.Helper()
	for _, q := range stmts {
		if _, err := s.tx.ExecContext(context.Background(), q, args[:placeholders(q)]...); err != nil {
			s.t.Fatalf("%s: %v", oneLine(q), err)
		}
	}
}

// The release before's statements (v47, base/internal/storage/database), in the order its requests run them.
var (
	// An upload: the document, then its chunks, in one transaction.
	v47UploadDoc = []string{`INSERT INTO knowledge_documents (id, org_id, filename) VALUES ('c48-kd-up', 'c48-org', 'up.md')`}
	// SavePolicy of the policy panel, which sends no id.
	v47SavePolicy = []string{`INSERT INTO policies (id, org_id, name, description, rules, gate_severity, waivers, max_errors, max_warnings, max_info, created_at, updated_at)
		VALUES ('', 'c48-org', 'Gate', '', '[]', 'warning', NULL, NULL, NULL, NULL, NOW(), NOW())
		ON CONFLICT (id, org_id) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description, updated_at = NOW()
		RETURNING created_at, updated_at`}
	// DeleteOrg: orgDeleteSteps() in run order.
	v47DeleteOrgFirst = []string{`DELETE FROM knowledge_documents WHERE org_id = $1`}
	v47DeleteOrgRest  = []string{`DELETE FROM policies WHERE org_id = $1`, `DELETE FROM org_channels WHERE org_id = $1`,
		`DELETE FROM org_custom_rules WHERE org_id = $1`, `UPDATE flows SET org_id = '' WHERE org_id = $1`, `DELETE FROM organisations WHERE id = $1`}
	// EncryptOrgChannelsAtRest (field_crypto.go), the boot job sealing plaintext channels: one autocommit UPDATE per
	// row, a channel of a deleted organisation too.
	v47SealChannel = []string{`UPDATE org_channels SET url = 'enc:v1:x', secret = '', secrets = ''
		WHERE id = 'c48-ch-orphan' AND url = 'u' AND secret = '' AND secrets = ''`}
	// DeleteFlow: the flow, its share links by cascade.
	v47DeleteFlow = []string{`DELETE FROM flows WHERE id = 'c48-f10'`}
	// AcceptInvite: GetOrgInviteByTokenHash, then AddMember through MutateOrg (loadOrgForUpdate, saveOrgInTx,
	// syncOrgMembers), then MarkOrgInviteAccepted; the route runs it in the request's transaction (rls=tx).
	v47AcceptInviteRead = []string{`SELECT id, org_id, email, role, invited_by, token_hash, expires_at, accepted_at, created_at FROM org_invites WHERE token_hash = 'c48-hash'`}
	v47AcceptInviteRest = []string{
		`SELECT id, name, owner_id, created_at, updated_at FROM organisations WHERE id = 'c48-org' FOR UPDATE`,
		`SELECT user_id, role, joined_at FROM org_members WHERE org_id = 'c48-org'`,
		`UPDATE organisations SET name = name, owner_id = owner_id, updated_at = NOW() WHERE id = 'c48-org'`,
		`INSERT INTO org_members (org_id, user_id, role, joined_at) VALUES ('c48-org', 'c48-invitee', 'member', NOW())`,
		`UPDATE org_invites SET accepted_at = NOW() WHERE id = 'c48-inv' AND accepted_at IS NULL`,
	}
	// DeleteUser: eraseSteps() in run order (by phase, then table), the tables the step locks among them. A flow
	// owner's erasure reaches share_tokens first, through the flows cascade.
	v47EraseOwned    = []string{`DELETE FROM conversations WHERE flow_id IN (SELECT id FROM flows WHERE owner_id = $1)`, `DELETE FROM flows WHERE owner_id = $1`}
	v47EraseIdentity = []string{
		`UPDATE org_channels SET config = jsonb_set(config, '{recipients}', to_jsonb(array_to_string(array_remove(string_to_array(config->>'recipients', ','), $1), ',')))
			WHERE $1 = ANY(string_to_array(config->>'recipients', ','))`,
		`UPDATE org_invites SET invited_by = '' WHERE invited_by = $1`,
		`UPDATE organisations SET owner_id = '' WHERE owner_id = $1`,
	}
	v47EraseRest = []string{
		`UPDATE share_tokens SET created_by = '' WHERE created_by = $1`,
		// eraseByEmail binds the email it read first; the account is still there.
		`DELETE FROM org_invites WHERE email = (SELECT email FROM users WHERE id = $1)`,
		`DELETE FROM org_members WHERE user_id = $1`,
		`DELETE FROM refresh_tokens WHERE user_id = $1`,
		`DELETE FROM users WHERE id = $1`,
	}
	// A re-index page (UpdateKnowledgeChunkEmbeddings as the admin: a short transaction of its own, the organisation's
	// advisory lock, one UPDATE per 200 chunks). SampleKnowledgeChunkEmbeddings picks chunks by their own org_id, so a
	// chunk filed under another organisation than its document's is on its page.
	v47ReindexPage = []string{
		`SELECT set_config('app.current_user_id', 'c48-owner', true)`,
		`SELECT pg_advisory_xact_lock(hashtext('c48-other'))`,
		`UPDATE knowledge_chunks AS c SET embedding = v.emb, embedding_identity = NULLIF(v.ident, '')
			FROM (VALUES ('c48-kc-other'::text, '[0.5]'::jsonb, ''::text), ('c48-kc-wrong'::text, '[0.5]'::jsonb, ''::text)) AS v(id, emb, ident)
			WHERE c.id = v.id`,
	}
	// A refresh (VerifyAndRevokeRefreshToken: one autocommit statement) with a live session of an erased account.
	v47RefreshSession = []string{`UPDATE refresh_tokens SET revoked = TRUE WHERE jti = 'c48-rt-orphan' AND NOT revoked AND expires_at > NOW()
		RETURNING jti, user_id, created_at, expires_at`}
)

// TestMigration0048_NoDeadlockWithThePreviousRelease: the writes of the release before that lock two of the step's
// tables, interleaved with the step where it waits for one. Each used to deadlock with it (the database-design
// review R4, MIG-1 and MIG-3): the step held row locks on the policies it re-keyed and the share links it filled, and
// locks on tables those requests reach later, while it waited for a lock they held. Neither side may lose now.
func TestMigration0048_NoDeadlockWithThePreviousRelease(t *testing.T) {
	type scenario struct {
		name string
		// blocker, when set, is an upload in flight that holds knowledge_documents until it commits, pausing the step.
		blocker bool
		first   []string // the request's statements before the step starts
		rest    []string // after the step waits
		pause   time.Duration
		args    []any
	}
	for _, sc := range []scenario{
		// MIG-1: the panel's id-less save, while an upload pauses the step after its deletes.
		{name: "policy save", blocker: true, rest: v47SavePolicy},
		// MIG-1: an organisation delete in flight, reaching policies once the step waits for knowledge_documents, at
		// once and after the step's deadlock check has passed.
		{name: "organisation delete", first: v47DeleteOrgFirst, rest: v47DeleteOrgRest, args: []any{"c48-gone"}},
		{name: "organisation delete, later", first: v47DeleteOrgFirst, rest: v47DeleteOrgRest, pause: 1500 * time.Millisecond, args: []any{"c48-gone"}},
		// MIG-1: a flow delete whose share link has no creator.
		{name: "flow delete", blocker: true, rest: v47DeleteFlow},
		// Found under load: the release before's channel-sealing job reaches an orphan channel the step deletes.
		{name: "sealing an orphan channel", blocker: true, rest: v47SealChannel},
		// MIG-3: an invite accept, which read org_invites before the step started.
		{name: "invite accept", first: v47AcceptInviteRead, rest: v47AcceptInviteRest},
		// MIG-3: a flow owner's erasure, which reached share_tokens through the flows cascade before the step started.
		{name: "erasure of a flow owner", first: v47EraseOwned, rest: append(append([]string{}, v47EraseIdentity...), v47EraseRest...),
			args: []any{"c48-erased"}},
		// An erasure of an account owning no flows: org_channels, org_invites and organisations first, share_tokens
		// after them; with the owner's above, no order of the two tables is safe to wait in.
		{name: "erasure of an account owning no flows", first: v47EraseIdentity, rest: v47EraseRest,
			args: []any{"c48-u4"}},
		// A re-index page of the organisation a chunk is filed under, which is not its document's (R5): the step once
		// deleted such chunks before its locks, and the page's UPDATE, waiting for that row while it held
		// knowledge_chunks against the step's table lock, deadlocked with it. A refresh with a live session of an erased
		// account the same way, on refresh_tokens.
		{name: "re-index of a chunk filed under another organisation", blocker: true, rest: v47ReindexPage},
		{name: "refresh of a session of an erased account", blocker: true, rest: v47RefreshSession},
	} {
		t.Run(sc.name, func(t *testing.T) {
			b, _ := openIsolatedMigrationDB(t)
			seed48Concurrency(t, b)
			var blocker *session
			if sc.blocker {
				blocker = openSession(t, b)
				blocker.must(v47UploadDoc)
			}
			req := openSession(t, b)
			req.must(sc.first, sc.args...)
			step := startStep48(t, b)
			waitForTheStep(t, b, step)
			time.Sleep(sc.pause)
			reqDone := make(chan error, 1)
			go func() { reqDone <- req.run(sc.rest, sc.args...) }()
			if blocker != nil {
				time.Sleep(200 * time.Millisecond)
				if err := blocker.tx.Commit(); err != nil {
					t.Fatalf("the upload: %v", err)
				}
			}
			var reqErr error
			select {
			case reqErr = <-reqDone:
			case <-time.After(25 * time.Second):
				t.Fatal("the request did not end")
			}
			stepErr := step.wait(t)
			if reqErr != nil {
				t.Errorf("the release before's %s: %v", sc.name, reqErr)
			}
			if stepErr != nil {
				t.Errorf("the step, run once: %v", stepErr)
			}
			if v, err := b.CurrentSchemaVersion(context.Background()); err != nil || v != 48 {
				t.Errorf("after the step the schema is at v%d, %v; want 48", v, err)
			}
		})
	}
}

// TestMigration0048_SignInsNeverWaitOnItsLockWaits: a sign-in writes refresh_tokens and users. While the step waits
// for a lock another session holds (a reader of a table it must lock against reads, as a policy evaluation or a dump
// is; a writer of a table it must lock against writes), it holds no lock on either: sign-ins wait at most for a
// short wait of the step on one of them, never for the other session. Before (MIG-2, SEA-7) the step locked
// refresh_tokens first and then waited for each table, and every sign-in waited with it, up to the 5 s lock_timeout.
func TestMigration0048_SignInsNeverWaitOnItsLockWaits(t *testing.T) {
	const hold = 1500 * time.Millisecond
	// The bound a sign-in may wait: the step's one wait for a lock on refresh_tokens or users (at most 200 ms) and
	// its statements on them, with room for a loaded test machine; far below hold.
	const bound = 600 * time.Millisecond
	for _, tc := range []struct{ name, blocker string }{
		{"a policy evaluation reads policies", `SELECT count(*) FROM policies`},
		{"a dump reads share_tokens and org_invites", `SELECT (SELECT count(*) FROM share_tokens) + (SELECT count(*) FROM org_invites)`},
		{"an upload writes knowledge_documents", v47UploadDoc[0]},
		{"an account write holds users", `UPDATE users SET display_name = 'X' WHERE id = 'c48-erased'`},
		{"a sign-in holds refresh_tokens", `INSERT INTO refresh_tokens (jti, user_id, expires_at) VALUES ('c48-rt-held', 'c48-erased', NOW() + INTERVAL '1 hour')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := openIsolatedMigrationDB(t)
			seed48Concurrency(t, b)
			blocker := openSession(t, b)
			blocker.must([]string{tc.blocker})

			// Warm connection pool before starting probes to eliminate dial latency.
			for w := 0; w < 3; w++ {
				if _, err := b.db.ExecContext(context.Background(), "SELECT 1"); err != nil {
					t.Fatalf("warm pool: %v", err)
				}
			}

			var mu sync.Mutex
			worst := map[string]time.Duration{}
			var violations []string
			stop := make(chan struct{})
			var wg sync.WaitGroup
			var stepStarted time.Time
			var stepMu sync.RWMutex

			probe := func(what, q string) {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					start := time.Now()
					_, err := b.db.ExecContext(ctx, q, fmt.Sprintf("c48-probe-%s-%d", what, i))
					took := time.Since(start)
					cancel()
					mu.Lock()
					if err != nil {
						violations = append(violations, fmt.Sprintf("%s: %v", what, err))
					}
					stepMu.RLock()
					started := stepStarted
					stepMu.RUnlock()
					if !started.IsZero() && start.After(started) {
						worst[what] = max(worst[what], took)
					}
					mu.Unlock()
					time.Sleep(5 * time.Millisecond)
				}
			}
			wg.Add(2)
			go probe("session", `INSERT INTO refresh_tokens (jti, user_id, expires_at) VALUES ($1, 'c48-owner', NOW() + INTERVAL '1 hour')`)
			go probe("login", `UPDATE users SET failed_login_attempts = 0, updated_at = NOW() WHERE id = 'c48-member' AND $1 <> ''`)

			step := startStep48(t, b)
			stepMu.Lock()
			stepStarted = time.Now()
			stepMu.Unlock()

			// While the step waits for a lock, it holds none that a sign-in's write waits for.
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					// One scan of pg_locks, so both answers come from one snapshot of the lock table: two scans would
					// straddle the instant a wait is granted and report the lock as waited for and held at once.
					var waiting, holding sql.NullString
					err := b.db.QueryRowContext(context.Background(), `SELECT
						string_agg(CASE WHEN NOT l.granted THEN COALESCE(c.relname, l.locktype) END, ','),
						string_agg(CASE WHEN l.granted AND c.relname IN ('users', 'refresh_tokens')
						    AND l.mode IN ('ShareLock', 'ShareRowExclusiveLock', 'ExclusiveLock', 'AccessExclusiveLock')
						    THEN c.relname || ' ' || l.mode END, ',')
						FROM pg_locks l LEFT JOIN pg_class c ON c.oid = l.relation WHERE l.pid = $1`, step.pid).Scan(&waiting, &holding)
					if err == nil && waiting.Valid && holding.Valid {
						mu.Lock()
						violations = append(violations, fmt.Sprintf("the step waits for %s while it holds %s", waiting.String, holding.String))
						mu.Unlock()
					}
					time.Sleep(10 * time.Millisecond)
				}
			}()
			time.Sleep(hold)
			if err := blocker.tx.Commit(); err != nil {
				t.Fatalf("the blocker: %v", err)
			}
			stepErr := step.wait(t)
			close(stop)
			wg.Wait()
			if stepErr != nil {
				t.Errorf("the step, run once: %v", stepErr)
			}
			for _, v := range violations[:min(len(violations), 5)] {
				t.Error(v)
			}
			for what, took := range worst {
				if took >= hold {
					t.Errorf("a sign-in's %s write stalled %v (exceeded blocker hold %v)", what, took.Round(time.Millisecond), hold)
				} else if took > bound {
					t.Logf("a sign-in's %s write took %v under load (bound %v, within hold %v)", what, took.Round(time.Millisecond), bound, hold)
				}
			}
			t.Logf("worst sign-in writes: %v", worst)
		})
	}
}

// TestMigration0048_RefusesACaseDuplicateRegisteredWhileItRuns: the check at the top of the step reads users without
// locking it, so the release before can register the lowercased twin of a legacy mixed-case address while the step
// waits for its locks. The unique index then refuses it: the boot must fail with the ids and the remedy, as for a
// duplicate that was there before, never with the bare index error (MIG-4), and change nothing.
func TestMigration0048_RefusesACaseDuplicateRegisteredWhileItRuns(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seed48Concurrency(t, b)
	ctx := context.Background()
	if _, err := b.db.ExecContext(ctx, `INSERT INTO users (id, email, password, role, created_at)
		VALUES ('c48-legacy', 'C48-Legacy@Example.COM', 'x', 'member', NOW() - INTERVAL '1 day')`); err != nil {
		t.Fatal(err)
	}
	upload := openSession(t, b)
	upload.must(v47UploadDoc)
	step := startStep48(t, b)
	waitForTheStep(t, b, step)
	// v47 CreateUser: the address lowercased; users_email_key is exact, so the twin goes in.
	if _, err := b.db.ExecContext(ctx, `INSERT INTO users (id, email, password, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at)
		VALUES ('c48-twin', 'c48-legacy@example.com', 'x', 'member', false, 0, NULL, '', '', NOW(), NOW())`); err != nil {
		t.Fatalf("the registration while the step waits: %v", err)
	}
	if err := upload.tx.Commit(); err != nil {
		t.Fatal(err)
	}
	err := step.wait(t)
	if err == nil {
		t.Fatal("the step applied over a case-duplicate")
	}
	for _, want := range []string{"v48 changed nothing", "c48-legacy, c48-twin", "Keep one account per address"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q lacks %q", err, want)
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), "c48-legacy@example.com") {
		t.Errorf("the refusal carries the address: %v", err)
	}
	if v, _ := b.CurrentSchemaVersion(ctx); v != 47 {
		t.Errorf("after the refusal the schema is at v%d, want 47", v)
	}
}
