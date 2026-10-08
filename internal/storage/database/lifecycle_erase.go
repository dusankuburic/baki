package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"pad-analyzer/internal/platform/txhook"
)

// erasePhase orders account erasure: the phases run in this order.
type erasePhase uint8

const (
	// eraseDetached: rows that hang off the user's content without a foreign key, before that content goes.
	eraseDetached erasePhase = iota
	// eraseOwned: what the user owns; ON DELETE CASCADE takes what hangs off it.
	eraseOwned
	// eraseIdentity: the user's identity is cleared on rows that stay (shared content, the audit trail).
	eraseIdentity
	// erasePersonal: the user's own rows that no foreign key cascades.
	erasePersonal
	// eraseAccount: the users row, last; its ON DELETE CASCADE foreign keys take the rest.
	eraseAccount
)

// eraseStep is one statement (or a few) of account erasure on one table.
type eraseStep struct {
	phase erasePhase
	table string
	// deletes says the step removes rows of table (rather than updating them), so ON DELETE CASCADE foreign keys to
	// table reach their children: the coverage test accepts a cascade claim only through such a parent.
	deletes bool
	run     func(ctx context.Context, b *PostgresStorageBackend, tx *sql.Tx, e *erasure) error
}

// erasure is the state of one account erasure.
type erasure struct {
	userID string
	// email and displayName are read before anything is deleted; "" when the account row is already gone.
	email, displayName string
	// after is the work that may run only once the erasure is durable (blob cleanup, the operator's log line).
	after []func(context.Context)
}

// eraseByUser is an erase step running query with $1 bound to the user's id.
func eraseByUser(phase erasePhase, table, query string) eraseStep {
	return eraseStep{phase: phase, table: table, deletes: isDelete(query), run: func(ctx context.Context, _ *PostgresStorageBackend, tx *sql.Tx, e *erasure) error {
		_, err := tx.ExecContext(ctx, query, e.userID)
		return err
	}}
}

// eraseByEmail is an erase step running query with $1 bound to the user's email; skipped when it is unknown.
func eraseByEmail(phase erasePhase, table, query string) eraseStep {
	return eraseStep{phase: phase, table: table, deletes: isDelete(query), run: func(ctx context.Context, _ *PostgresStorageBackend, tx *sql.Tx, e *erasure) error {
		if e.email == "" {
			return nil
		}
		_, err := tx.ExecContext(ctx, query, e.email)
		return err
	}}
}

// isDelete reports whether query is a DELETE statement.
func isDelete(query string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "DELETE")
}

// DeleteUser performs GDPR-style account erasure for userID: every domain's erase steps (lifecycle_*.go) delete the
// user's owned flows and per-user rows and clear the user's identity on the rows kept for others (shared comments,
// triage, versions, share links, invites, the audit trail). The statements run without the row-level-security user,
// joining the request's transaction when there is one: they are keyed by the user's id and must reach the user's
// rows on flows the user can no longer see; the caller authenticated the user. Idempotent: a missing user is not an
// error. Blob cleanup runs after the commit.
func (b *PostgresStorageBackend) DeleteUser(ctx context.Context, userID string) error {
	e := &erasure{userID: userID}
	if err := b.withTx(ctx, func(_ context.Context, tx *sql.Tx) error {
		return b.eraseUserInTx(ctx, tx, e)
	}); err != nil {
		return err
	}
	// Registered on ctx, not the transaction's context: when this request carries the middleware's transaction,
	// withTx joined it and nothing is durable until the middleware commits.
	for _, fn := range e.after {
		txhook.Register(ctx, fn)
	}
	return nil
}

// eraseUserInTx runs the erase steps inside tx.
func (b *PostgresStorageBackend) eraseUserInTx(ctx context.Context, tx *sql.Tx, e *erasure) error {
	return asSystem(ctx, tx, func() error {
		// The email and display name before the row goes: org_invites is keyed by email, and teammates'
		// notifications carry one or the other. "No rows" is the idempotent case; the steps still clean up residue.
		err := tx.QueryRowContext(ctx, `SELECT email, display_name FROM users WHERE id = $1`, e.userID).Scan(&e.email, &e.displayName)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("erasure: read user: %w", err)
		}
		for _, s := range eraseSteps() {
			if err := s.run(ctx, b, tx, e); err != nil {
				return fmt.Errorf("erasure: %s: %w", s.table, err)
			}
		}
		return nil
	})
}

// orgPhase orders organisation deletion: the phases run in this order.
type orgPhase uint8

const (
	// orgContent: the organisation's configuration and content that no foreign key cascades.
	orgContent orgPhase = iota
	// orgDetach: rows that outlive the organisation lose its id.
	orgDetach
	// orgRow: the organisations row; its ON DELETE CASCADE foreign keys take members, invites, settings,
	// credentials and the Azure registry.
	orgRow
)

// orgDeleteStep is one statement of organisation deletion; $1 is the organisation's id.
type orgDeleteStep struct {
	phase orgPhase
	table string
	sql   string
}

// deleteOrgInTx runs the org-delete steps of every domain inside tx, without the row-level-security user: the
// statements are keyed by the organisation's id, the caller is authorised for it, and the organisation's flows that
// other members own must still be detached (their policy would refuse the admin that update).
func (b *PostgresStorageBackend) deleteOrgInTx(ctx context.Context, tx *sql.Tx, orgID string) error {
	return asSystem(ctx, tx, func() error {
		for _, s := range orgDeleteSteps() {
			if _, err := tx.ExecContext(ctx, s.sql, orgID); err != nil {
				return fmt.Errorf("delete org: %s: %w", s.table, err)
			}
		}
		return nil
	})
}
