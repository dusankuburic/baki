package database

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// The data lifecycle is what the data-subject export, account erasure, organisation deletion and the retention
// sweep do with each table. Every domain declares it for its own tables in a lifecycle_<domain>.go file, registered
// from init like the analyzer's rules; the runners order the steps by their phase, never by file. A table no domain
// claims fails TestPostgres_Lifecycle_EveryTableClaimed, so a new table cannot be forgotten by any of them (ADR 0011).
//
// Everything lives in this package because every step is SQL inside the erasure's or the sweep's transaction and
// every table is created by this package's migrations; a contribution from outside would need the transaction and
// the step types exported for no current user. The service-level part of erasure (releasing credentials, the lent
// organisation credentials, AccountErased) stays in AccountLifecycleService.
type lifecycleDomain struct {
	name      string
	tables    []tableClaim
	export    []exportSection
	erase     []eraseStep
	orgDelete []orgDeleteStep
	purge     []purgeStep
}

// tableClaim is one table's classification for every lifecycle operation. Each choice that leaves the table alone
// carries its reason.
type tableClaim struct {
	table string
	// export is the bundle section (a json key of interfaces.UserDataExport) carrying the user's rows; noExport says
	// why the bundle has none.
	export, noExport string
	erase            eraseHow
	// eraseWhy says why a notPersonal table holds nothing of the user's.
	eraseWhy string
	org      orgHow
	// orgWhy says why orgKept rows keep the organisation's id.
	orgWhy string
	// noPurge says why the retention sweep leaves the table alone; "" when a purge step covers it.
	noPurge string
	// legacy names the legacy migrator's step (internal/migration) that copies the table's data from a desktop data
	// directory, if one does.
	legacy string
}

// eraseHow is how account erasure reaches a table's rows about the user (flags, except notPersonal).
type eraseHow uint8

const (
	// erasedByStep: an erase step of the table deletes the user's rows or clears their identity on rows that stay.
	erasedByStep eraseHow = 1 << iota
	// erasedByCascade: the user's rows go with a parent the erasure deletes (the account, an owned flow) through an
	// ON DELETE CASCADE foreign key.
	erasedByCascade
	// notPersonal: the table holds no user identity.
	notPersonal
)

// orgHow is what deleting an organisation does with a table's rows of that organisation.
type orgHow uint8

const (
	// orgNotKeyed: the table has no organisation column.
	orgNotKeyed orgHow = iota + 1
	// orgByStep: an org-delete step of the table deletes the rows or detaches them from the organisation.
	orgByStep
	// orgByCascade: the rows go with the organisation (or an org-owned parent) through ON DELETE CASCADE.
	orgByCascade
	// orgKept: the rows keep the organisation's id as history.
	orgKept
)

// lifecycleDomains accumulates every domain's declaration through registerLifecycle; only init writes it.
var lifecycleDomains []lifecycleDomain

// registerLifecycle adds a domain's declaration. Called from the lifecycle files' init.
func registerLifecycle(d lifecycleDomain) {
	lifecycleDomains = append(lifecycleDomains, d)
}

// tableClaims returns every claim, in table order.
func tableClaims() []tableClaim {
	var out []tableClaim
	for _, d := range lifecycleDomains {
		out = append(out, d.tables...)
	}
	slices.SortStableFunc(out, func(a, b tableClaim) int { return cmp.Compare(a.table, b.table) })
	return out
}

// eraseSteps returns the erase steps in run order: by phase, then by table (the steps of one phase must not depend
// on each other).
func eraseSteps() []eraseStep {
	var out []eraseStep
	for _, d := range lifecycleDomains {
		out = append(out, d.erase...)
	}
	slices.SortStableFunc(out, func(a, b eraseStep) int {
		return cmp.Or(cmp.Compare(a.phase, b.phase), cmp.Compare(a.table, b.table))
	})
	return out
}

// orgDeleteSteps returns the organisation-delete steps in run order, as eraseSteps.
func orgDeleteSteps() []orgDeleteStep {
	var out []orgDeleteStep
	for _, d := range lifecycleDomains {
		out = append(out, d.orgDelete...)
	}
	slices.SortStableFunc(out, func(a, b orgDeleteStep) int {
		return cmp.Or(cmp.Compare(a.phase, b.phase), cmp.Compare(a.table, b.table))
	})
	return out
}

// asSystem runs fn with the transaction's row-level-security user cleared, so the policies admit every row, and
// puts the user back afterwards. It is for lifecycle statements keyed by one user's or one organisation's id that
// must reach all of its rows whoever's flow they hang off: erasing a comment on a flow the user can no longer see,
// detaching an organisation's flows from it. The caller is authorised for that id at the service. On the pool
// (a *sql.DB) no user is set, so fn runs as it is.
func asSystem(ctx context.Context, q DBTX, fn func() error) (err error) {
	tx, ok := q.(*sql.Tx)
	if !ok {
		return fn()
	}
	var user string
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(current_setting('app.current_user_id', true), '')`).Scan(&user); err != nil {
		return fmt.Errorf("read the rls user: %w", err)
	}
	if user == "" {
		return fn()
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_user_id', '', true)`); err != nil {
		return fmt.Errorf("clear the rls user: %w", err)
	}
	// Deferred so a panic in fn cannot leave the rest of the request's transaction unscoped.
	defer func() {
		if _, rerr := tx.ExecContext(ctx, `SELECT set_config('app.current_user_id', $1, true)`, user); rerr != nil && err == nil {
			err = fmt.Errorf("restore the rls user: %w", rerr)
		}
	}()
	return fn()
}

// rowsOfStrings reads the first column of a query into a []string.
func rowsOfStrings(ctx context.Context, q DBTX, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func init() {
	registerLifecycle(lifecycleDomain{
		name: "schema",
		tables: []tableClaim{
			{table: "schema_migrations", noExport: "schema bookkeeping", erase: notPersonal, eraseWhy: "schema bookkeeping",
				org: orgNotKeyed, noPurge: "one row per migration step"},
		},
	})
}
