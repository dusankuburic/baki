package database

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	legacymigration "pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/interfaces"
)

// legacyMigratorSteps are the names of the legacy migrator's steps that copy data (the ones with a Run); a claim's
// legacy field names one of them.
func legacyMigratorSteps() []string {
	var names []string
	for _, s := range legacymigration.Steps() {
		if s.Run != nil {
			names = append(names, s.Name)
		}
	}
	return names
}

// TestLifecycle_ClaimsAreConsistent checks every table claim against the steps the domains register: a claim that
// says a step handles the table has one, a step runs only on a table that says so, and every choice that leaves a
// table alone gives its reason.
func TestLifecycle_ClaimsAreConsistent(t *testing.T) {
	claims := map[string]tableClaim{}
	for _, c := range tableClaims() {
		if _, dup := claims[c.table]; dup {
			t.Errorf("%s is claimed twice", c.table)
		}
		claims[c.table] = c
	}
	sections := map[string]bool{}
	for _, s := range exportSections() {
		sections[s.key] = true
	}
	stepped := func(tables []string) map[string]bool {
		out := map[string]bool{}
		for _, table := range tables {
			if _, ok := claims[table]; !ok {
				t.Errorf("a step runs on %s, which no domain claims", table)
			}
			out[table] = true
		}
		return out
	}
	var eraseTables, orgTables, purgeTables []string
	for _, s := range eraseSteps() {
		eraseTables = append(eraseTables, s.table)
	}
	for _, s := range orgDeleteSteps() {
		orgTables = append(orgTables, s.table)
	}
	for _, s := range purgeSteps() {
		purgeTables = append(purgeTables, s.table)
	}
	erased, orgDeleted, purged := stepped(eraseTables), stepped(orgTables), stepped(purgeTables)

	for _, c := range claims {
		switch {
		case (c.export == "") == (c.noExport == ""):
			t.Errorf("%s: name the export section carrying it or say why the bundle has none (one of the two)", c.table)
		case c.export != "" && !sections[c.export]:
			t.Errorf("%s: export section %q is not a section of the bundle", c.table, c.export)
		}
		switch {
		case c.erase == 0:
			t.Errorf("%s: say how account erasure reaches it", c.table)
		case c.erase&notPersonal != 0 && (c.erase != notPersonal || c.eraseWhy == ""):
			t.Errorf("%s: notPersonal stands alone, with eraseWhy", c.table)
		case c.erase&notPersonal == 0 && c.eraseWhy != "":
			t.Errorf("%s: eraseWhy explains notPersonal only", c.table)
		}
		if has := c.erase&erasedByStep != 0; has != erased[c.table] {
			t.Errorf("%s: claims erasedByStep = %v, but an erase step on it exists = %v", c.table, has, erased[c.table])
		}
		switch {
		case c.org == 0:
			t.Errorf("%s: say what deleting an organisation does with it", c.table)
		case (c.org == orgKept) != (c.orgWhy != ""):
			t.Errorf("%s: orgWhy explains orgKept, and only it", c.table)
		}
		if has := c.org == orgByStep; has != orgDeleted[c.table] {
			t.Errorf("%s: claims orgByStep = %v, but an org-delete step on it exists = %v", c.table, has, orgDeleted[c.table])
		}
		if has := c.noPurge == ""; has != purged[c.table] {
			t.Errorf("%s: a purge step exists = %v, but noPurge = %q (empty exactly when a step purges it)", c.table, purged[c.table], c.noPurge)
		}
		if c.legacy != "" && !slices.Contains(legacyMigratorSteps(), c.legacy) {
			t.Errorf("%s: legacy names %q, not a step of the legacy migrator %v", c.table, c.legacy, legacyMigratorSteps())
		}
	}
}

// TestLifecycle_ExportSectionsAreTheBundle: the sections are exactly interfaces.UserDataExport's fields, in its order
// and with its types, so the streamed bundle (written section by section) and the buffered one (the struct) carry
// the same keys in the same order.
func TestLifecycle_ExportSectionsAreTheBundle(t *testing.T) {
	typ := reflect.TypeFor[interfaces.UserDataExport]()
	sections := exportSections()
	var got, want []string
	for _, s := range sections {
		got = append(got, s.key)
	}
	for i := range typ.NumField() {
		key, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		want = append(want, key)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("export sections %v, the bundle's keys %v", got, want)
	}
	for i, s := range sections {
		if f := typ.Field(i); s.typ != f.Type {
			t.Errorf("section %q is a %v, the bundle's field %s a %v", s.key, s.typ, f.Name, f.Type)
		}
	}
}

// TestLifecycle_PurgeStepsReportEveryResultField: each PurgeResult field is reported by exactly one purge step, so
// the sweep's JSON and its log line cover every table it purges.
func TestLifecycle_PurgeStepsReportEveryResultField(t *testing.T) {
	var r interfaces.PurgeResult
	v := reflect.ValueOf(&r).Elem()
	fieldAt := map[uintptr]string{}
	for i := range v.NumField() {
		fieldAt[v.Field(i).Addr().Pointer()] = v.Type().Field(i).Name
	}
	reportedBy := map[string]string{}
	for _, s := range purgeSteps() {
		name, ok := fieldAt[reflect.ValueOf(s.count(&r)).Pointer()]
		switch {
		case !ok:
			t.Errorf("the %s purge step reports into no PurgeResult field", s.table)
		case reportedBy[name] != "":
			t.Errorf("PurgeResult.%s is reported by both %s and %s", name, reportedBy[name], s.table)
		default:
			reportedBy[name] = s.table
		}
	}
	for _, name := range fieldAt {
		if reportedBy[name] == "" {
			t.Errorf("no purge step reports PurgeResult.%s", name)
		}
	}
}

// TestLifecycle_StepsRunInPhaseOrder pins the orderings erasure and organisation deletion depend on.
func TestLifecycle_StepsRunInPhaseOrder(t *testing.T) {
	index := func(steps []eraseStep, table string, phase erasePhase) int {
		return slices.IndexFunc(steps, func(s eraseStep) bool { return s.table == table && s.phase == phase })
	}
	steps := eraseSteps()
	// Conversations have no foreign key to flows: they must go before the flows they hang off.
	if c, f := index(steps, "conversations", eraseDetached), index(steps, "flows", eraseOwned); c < 0 || f < 0 || c > f {
		t.Errorf("conversations (%d) must be erased before the owned flows (%d)", c, f)
	}
	// The organisations a user is the last admin of are read before the memberships go.
	if o, m := index(steps, "organisations", eraseIdentity), index(steps, "org_members", erasePersonal); o < 0 || m < 0 || o > m {
		t.Errorf("organisations (%d) must be handled before org_members (%d)", o, m)
	}
	if last := steps[len(steps)-1]; last.table != "users" {
		t.Errorf("the last erase step is %s, want the users row", last.table)
	}
	org := orgDeleteSteps()
	if last := org[len(org)-1]; last.table != "organisations" || last.phase != orgRow {
		t.Errorf("the last org-delete step is %s, want the organisations row", last.table)
	}
	for i := 1; i < len(steps); i++ {
		if steps[i].phase < steps[i-1].phase {
			t.Fatalf("erase steps out of phase order at %s", steps[i].table)
		}
	}
}

// TestPostgres_Lifecycle_EveryTableClaimed is the coverage test: every table of the migrated schema is claimed by
// exactly one domain, a cascade claim is backed by an ON DELETE CASCADE foreign key to a parent the operation
// removes, and the organisation classification matches whether the table has an org_id column. A new table fails
// here until its domain declares what the export, erasure, organisation deletion and the retention sweep do with it.
func TestPostgres_Lifecycle_EveryTableClaimed(t *testing.T) {
	// A private copy of the migrated schema: a table another test creates on the shared database is not the schema's.
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()

	tables, err := rowsOfStrings(ctx, b.db, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p')
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(tables) < 30 {
		t.Fatalf("found %d tables; have the migrations run?", len(tables))
	}
	claims := map[string]tableClaim{}
	for _, c := range tableClaims() {
		claims[c.table] = c
	}
	for _, table := range tables {
		if _, ok := claims[table]; !ok {
			t.Errorf("table %s is not claimed: declare in a lifecycle_<domain>.go file what the data-subject export, "+
				"account erasure, organisation deletion and the retention sweep do with it", table)
		}
	}
	for table := range claims {
		if !slices.Contains(tables, table) {
			t.Errorf("%s is claimed but the schema has no such table", table)
		}
	}

	rows, err := b.db.QueryContext(ctx, `
		SELECT conrelid::regclass::text, confrelid::regclass::text
		  FROM pg_constraint
		 WHERE contype = 'f' AND confdeltype = 'c' AND connamespace = current_schema()::regnamespace`)
	if err != nil {
		t.Fatalf("list foreign keys: %v", err)
	}
	defer rows.Close()
	cascadesFrom := map[string][]string{}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			t.Fatal(err)
		}
		cascadesFrom[child] = append(cascadesFrom[child], parent)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// A cascade claim holds only through a parent whose rows the operation really deletes: one a step DELETEs from,
	// or, transitively, one the cascade itself empties. A parent the operation only updates (an organisation losing
	// its owner, a flow detached from its organisation) cascades nothing.
	erased, orgDeleted := map[string]bool{}, map[string]bool{}
	for _, s := range eraseSteps() {
		if s.deletes {
			erased[s.table] = true
		}
	}
	for _, s := range orgDeleteSteps() {
		if isDelete(s.sql) {
			orgDeleted[s.table] = true
		}
	}
	reach := func(deleted map[string]bool, cascades func(tableClaim) bool) {
		for grew := true; grew; {
			grew = false
			for _, c := range claims {
				if !deleted[c.table] && cascades(c) && slices.ContainsFunc(cascadesFrom[c.table], func(p string) bool { return deleted[p] }) {
					deleted[c.table], grew = true, true
				}
			}
		}
	}
	reach(erased, func(c tableClaim) bool { return c.erase&erasedByCascade != 0 })
	reach(orgDeleted, func(c tableClaim) bool { return c.org == orgByCascade })
	for _, c := range claims {
		if c.erase&erasedByCascade != 0 && !erased[c.table] {
			t.Errorf("%s claims erasedByCascade, but no ON DELETE CASCADE foreign key leads to a table erasure deletes from (%v)", c.table, cascadesFrom[c.table])
		}
		if c.org == orgByCascade && !orgDeleted[c.table] {
			t.Errorf("%s claims orgByCascade, but no ON DELETE CASCADE foreign key leads to a table organisation deletion deletes from (%v)", c.table, cascadesFrom[c.table])
		}
	}

	orgKeyed, err := rowsOfStrings(ctx, b.db, `
		SELECT table_name FROM information_schema.columns
		 WHERE table_schema = current_schema() AND column_name = 'org_id'`)
	if err != nil {
		t.Fatal(err)
	}
	orgKeyed = append(orgKeyed, "organisations")
	for _, c := range claims {
		if keyed := slices.Contains(orgKeyed, c.table); keyed == (c.org == orgNotKeyed) {
			t.Errorf("%s: has an organisation column = %v, but claims orgNotKeyed = %v", c.table, keyed, c.org == orgNotKeyed)
		}
	}
}
