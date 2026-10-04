package database

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"pad-analyzer/internal/storage/interfaces"
)

// Tests for the fourth performance round's save statement (saveFlowSQL): the
// content is bound once, the flow_search body is derived in SQL from the written
// row (PAD source text, else JSON text, "{}" placeholder in blob mode), and the
// search row is rewritten only when its name or body changed. DB tests are
// DATABASE_URL-gated (throwaway DB).

// saveFlowSearchBodyExpr is the body expression saveFlowSQL derives from the
// written row.
const saveFlowSearchBodyExpr = "COALESCE(NULLIF(up.content->>'source', ''), up.content::text)"

// maxPlaceholder returns the highest $n in q.
func maxPlaceholder(q string) int {
	max := 0
	for _, m := range regexp.MustCompile(`\$(\d+)`).FindAllStringSubmatch(q, -1) {
		if n, _ := strconv.Atoi(m[1]); n > max {
			max = n
		}
	}
	return max
}

// TestSaveFlowSQL_BindsContentOnceAndGuardsTheSearchRewrite pins the
// statement's shape without a database: content bound once ($4), body derived
// from the inserted row, search row rewritten only on a real change, and the
// result row from the flows CTE — never the guarded search write, whose empty
// RETURNING on a no-op would read as a version conflict.
func TestSaveFlowSQL_BindsContentOnceAndGuardsTheSearchRewrite(t *testing.T) {
	one := oneLine(saveFlowSQL)
	if got := maxPlaceholder(one); got != 11 {
		t.Errorf("highest placeholder is $%d, want $11: the content must be bound once (as $4), not again as the search body", got)
	}
	if n := strings.Count(one, "$4"); n != 1 {
		t.Errorf("the content parameter $4 is referenced %d times, want once", n)
	}
	if !strings.Contains(one, "RETURNING id, name, content, version ), fs AS ( INSERT INTO flow_search (flow_id, name, body) SELECT up.id, up.name, "+saveFlowSearchBodyExpr+" FROM up") {
		t.Errorf("the search body must be derived from the written row as %s:\n%s", saveFlowSearchBodyExpr, one)
	}
	if !strings.Contains(one, "content->>'source'") {
		t.Errorf("the body must prefer the document's PAD source text (content->>'source'):\n%s", one)
	}
	if !strings.Contains(one, "ON CONFLICT (flow_id) DO UPDATE SET name = EXCLUDED.name, body = EXCLUDED.body WHERE flow_search.name IS DISTINCT FROM EXCLUDED.name OR flow_search.body IS DISTINCT FROM EXCLUDED.body") {
		t.Errorf("the search rewrite must be guarded by IS DISTINCT FROM on name and body:\n%s", one)
	}
	if !strings.HasSuffix(one, ") SELECT version FROM up") {
		t.Errorf("the result row must come from the flows CTE (SELECT version FROM up):\n%s", one)
	}
	if search := one[strings.Index(one, "INSERT INTO flow_search"):]; strings.Contains(search, "RETURNING") {
		t.Errorf("the search write must not RETURN: skipped by its guard, an empty RETURNING reads as sql.ErrNoRows → ErrVersionConflict:\n%s", search)
	}
	if !strings.Contains(one, "WHERE flows.version = $11") {
		t.Errorf("the OCC check must stay on the flows upsert:\n%s", one)
	}
}

// TestSaveFlow_BindsContentOnce_BodyFollowsStorageMode: through the fake
// driver, the save binds eleven arguments — content exactly once, as $4 — and
// in blob mode that content is the "{}" placeholder. The fake fails the
// statement, so the never-contacted blob client sees no upload.
func TestSaveFlow_BindsContentOnce_BodyFollowsStorageMode(t *testing.T) {
	client, err := azblob.NewClientFromConnectionString(azuriteDefaultConnStr, nil)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	content := []byte(`{"blocks":[{"name":"SetVariable zebra"}]}`)
	for _, tc := range []struct {
		name        string
		blob        *azblob.Client
		wantContent string
	}{
		{"inline mode binds the content", nil, string(content)},
		{"blob mode binds the placeholder", client, "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := errors.New("stop before any upload")
			b, f := newFakeBackend(t, 1, func(query string, _ []driver.NamedValue) fakeResult {
				if strings.Contains(query, "INSERT INTO flow_search") {
					return fakeResult{err: stop}
				}
				return fakeResult{}
			})
			b.blobClient, b.container = tc.blob, "search-body-test"

			err := b.SaveFlow(context.Background(), &interfaces.FlowDocument{ID: "f1", Name: "Alpha", OwnerID: "u1", Content: content})
			if !errors.Is(err, stop) {
				t.Fatalf("SaveFlow err = %v, want the scripted stop", err)
			}
			var save *fakeCall
			for _, c := range f.Queries() {
				if strings.Contains(c.query, "INSERT INTO flow_search") {
					c := c
					save = &c
				}
			}
			if save == nil {
				t.Fatal("save statement not sent")
			}
			if len(save.args) != 11 {
				t.Fatalf("save statement has %d args, want 11 (no separate search-body parameter)", len(save.args))
			}
			if got, _ := save.args[1].Value.(string); got != "Alpha" {
				t.Errorf("name arg ($2) = %q, want Alpha", got)
			}
			if got, _ := save.args[3].Value.(string); got != tc.wantContent {
				t.Errorf("content arg ($4) = %q, want %q", got, tc.wantContent)
			}
			for i, a := range save.args {
				if s, ok := a.Value.(string); ok && i != 3 && s == string(content) {
					t.Errorf("the content is bound a second time as $%d", i+1)
				}
			}
		})
	}
}

// r4FlowSearchState is what the DB tests read back after a save: the flows
// version, the flow_search row's xmin (unchanged ⇒ not rewritten) and its body.
type r4FlowSearchState struct {
	version int
	xmin    string
	name    string
	body    string
}

func r4SearchState(t *testing.T, b *PostgresStorageBackend, id string) r4FlowSearchState {
	t.Helper()
	var s r4FlowSearchState
	if err := b.db.QueryRowContext(context.Background(), `
		SELECT f.version, s.xmin::text, s.name, s.body FROM flows f JOIN flow_search s ON s.flow_id = f.id WHERE f.id = $1`, id).
		Scan(&s.version, &s.xmin, &s.name, &s.body); err != nil {
		t.Fatalf("read flow + search row %s: %v", id, err)
	}
	return s
}

// r4SaveCounting runs SaveFlow in one transaction and reports, from inside
// that transaction, how many flow_search rows the save inserted and updated:
// pg_stat_xact_user_tables is exact and immediate, but pending counts of
// earlier transactions on the same pooled connection may await an idle flush,
// so the save is measured as the difference of two reads inside the one
// transaction.
func r4SaveCounting(t *testing.T, b *PostgresStorageBackend, doc *interfaces.FlowDocument) (ins, upd int, err error) {
	t.Helper()
	ctx := context.Background()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	pending := func() (ins, upd int) {
		if err := tx.QueryRowContext(ctx, `SELECT n_tup_ins, n_tup_upd FROM pg_stat_xact_user_tables WHERE relname = 'flow_search'`).Scan(&ins, &upd); err != nil {
			t.Fatalf("pg_stat_xact_user_tables: %v", err)
		}
		return ins, upd
	}
	ins0, upd0 := pending()
	if err := b.SaveFlow(WithRLSTx(ctx, tx), doc); err != nil {
		return 0, 0, err
	}
	ins1, upd1 := pending()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return ins1 - ins0, upd1 - upd0, nil
}

// TestSaveFlow_UnchangedSearchRowIsNotRewritten: a save that changes neither
// name nor body advances the flows version but leaves flow_search untouched
// (same xmin, zero updates); a content or name change rewrites it; a stale
// version is ErrVersionConflict and touches nothing.
func TestSaveFlow_UnchangedSearchRowIsNotRewritten(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	id := "r4-search-" + time.Now().Format("150405.000000000")
	t.Cleanup(func() { _ = b.DeleteFlow(ctx, id) })

	doc := &interfaces.FlowDocument{ID: id, Name: "Alpha", OwnerID: "r4-owner", Description: "first", Content: []byte(`{"k":"v1"}`)}
	ins, upd, err := r4SaveCounting(t, b, doc)
	if err != nil || ins != 1 || upd != 0 {
		t.Fatalf("first save: ins=%d upd=%d err=%v; want one search insert", ins, upd, err)
	}
	s0 := r4SearchState(t, b, id)
	if s0.version != 0 || s0.body != `{"k": "v1"}` {
		t.Fatalf("after the first save: %+v", s0)
	}

	// No-op for the search row: description edit, same name and content.
	doc.Version, doc.Description = s0.version, "second"
	if ins, upd, err = r4SaveCounting(t, b, doc); err != nil || ins != 0 || upd != 0 {
		t.Errorf("description-only save: ins=%d upd=%d err=%v; want no search write", ins, upd, err)
	}
	s1 := r4SearchState(t, b, id)
	if s1.version != s0.version+1 {
		t.Errorf("flows.version = %d after the description-only save, want %d", s1.version, s0.version+1)
	}
	if s1.xmin != s0.xmin {
		t.Errorf("flow_search was rewritten by a description-only save (xmin %s → %s)", s0.xmin, s1.xmin)
	}

	// Same JSON, other encoding: the body is the normalised text, still equal.
	doc.Version, doc.Content = s1.version, []byte("{\"k\":   \"v1\"}")
	if ins, upd, err = r4SaveCounting(t, b, doc); err != nil || ins != 0 || upd != 0 {
		t.Errorf("re-encoded same content: ins=%d upd=%d err=%v; want no search write", ins, upd, err)
	}
	s2 := r4SearchState(t, b, id)
	if s2.xmin != s1.xmin || s2.version != s1.version+1 {
		t.Errorf("re-encoded same content: version %d→%d, xmin %s→%s; want the version bumped and the search row kept", s1.version, s2.version, s1.xmin, s2.xmin)
	}

	// A content change rewrites the body.
	doc.Version, doc.Content = s2.version, []byte(`{"k":"v2"}`)
	if ins, upd, err = r4SaveCounting(t, b, doc); err != nil || ins != 0 || upd != 1 {
		t.Errorf("content change: ins=%d upd=%d err=%v; want one search update", ins, upd, err)
	}
	s3 := r4SearchState(t, b, id)
	if s3.xmin == s2.xmin || s3.body != `{"k": "v2"}` {
		t.Errorf("content change did not rewrite the search row: %+v (xmin before %s)", s3, s2.xmin)
	}

	// A rename alone rewrites it too (the name is indexed for the library).
	doc.Version, doc.Name = s3.version, "Beta"
	if ins, upd, err = r4SaveCounting(t, b, doc); err != nil || ins != 0 || upd != 1 {
		t.Errorf("rename: ins=%d upd=%d err=%v; want one search update", ins, upd, err)
	}
	s4 := r4SearchState(t, b, id)
	if s4.xmin == s3.xmin || s4.name != "Beta" {
		t.Errorf("rename did not rewrite the search row: %+v (xmin before %s)", s4, s3.xmin)
	}

	// Stale version: refused, nothing written.
	doc.Version, doc.Name, doc.Content = 0, "Stale", []byte(`{"k":"stale"}`)
	if _, _, err = r4SaveCounting(t, b, doc); !errors.Is(err, interfaces.ErrVersionConflict) {
		t.Errorf("stale save err = %v, want ErrVersionConflict", err)
	}
	if s5 := r4SearchState(t, b, id); s5 != s4 {
		t.Errorf("a rejected writer changed the row: %+v → %+v", s4, s5)
	}
}

// TestSaveFlow_SearchBodyIsTheSourceText: a document carrying its PAD source
// (content.source) is indexed by that text, not the larger JSON, and the
// content search finds it; a document without a source keeps the JSON body.
func TestSaveFlow_SearchBodyIsTheSourceText(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000000")
	owner := "r4-src-owner-" + stamp
	withSource, withoutSource := "r4-src-a-"+stamp, "r4-src-b-"+stamp
	t.Cleanup(func() {
		_ = b.DeleteFlow(ctx, withSource)
		_ = b.DeleteFlow(ctx, withoutSource)
	})
	const source = "Display.ShowMessageBox Message: 'needle-in-source'"
	content := []byte(`{"blocks":[{"name":"Display.ShowMessageBox"}],"source":"` + source + `"}`)
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: withSource, Name: "with source", OwnerID: owner, Content: content}); err != nil {
		t.Fatalf("SaveFlow (with source): %v", err)
	}
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: withoutSource, Name: "without source", OwnerID: owner, Content: []byte(`{"blocks":[{"name":"SetVariable zebra"}]}`)}); err != nil {
		t.Fatalf("SaveFlow (without source): %v", err)
	}
	if got := r4SearchState(t, b, withSource); got.body != source {
		t.Errorf("body of the flow with a source = %q, want the source text %q", got.body, source)
	}
	if got := r4SearchState(t, b, withoutSource); got.body != `{"blocks": [{"name": "SetVariable zebra"}]}` {
		t.Errorf("body of the flow without a source = %q, want its JSON text", got.body)
	}
	hits, err := b.SearchFlowContents(ctx, interfaces.FlowFilter{UserID: owner}, "needle-in-source", 10)
	if err != nil {
		t.Fatalf("SearchFlowContents: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != withSource {
		t.Errorf("SearchFlowContents(needle-in-source) = %d hits %v, want exactly %s", len(hits), flowIDs(hits), withSource)
	}
	// An empty source falls back to the JSON text, not an empty body.
	empty := "r4-src-c-" + stamp
	t.Cleanup(func() { _ = b.DeleteFlow(ctx, empty) })
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: empty, Name: "empty source", OwnerID: owner, Content: []byte(`{"source":"","k":"v"}`)}); err != nil {
		t.Fatalf("SaveFlow (empty source): %v", err)
	}
	if got := r4SearchState(t, b, empty); got.body != `{"k": "v", "source": ""}` {
		t.Errorf("body of the flow with an empty source = %q, want its JSON text", got.body)
	}
}

func flowIDs(docs []*interfaces.FlowDocument) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.ID)
	}
	return out
}

// explainAsUserWith is explainAsUser with further SET LOCAL settings applied
// first, e.g. "SET LOCAL enable_sort = off" to ask whether an index can deliver
// an ORDER BY on its own.
func explainAsUserWith(t *testing.T, b *PostgresStorageBackend, userID, q string, args []any, settings ...string) []planNode {
	t.Helper()
	ctx := context.Background()
	tx, err := b.BeginRLS(ctx, userID)
	if err != nil {
		t.Fatalf("BeginRLS: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range append([]string{`SET LOCAL enable_seqscan = off`}, settings...) {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	rows, err := tx.QueryContext(ctx, "EXPLAIN (FORMAT JSON) "+q, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\n%s", err, q)
	}
	defer rows.Close()
	var raw string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain: %v", err)
		}
		raw += line
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	var top []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatalf("parse explain json: %v\n%s", err, raw)
	}
	if len(top) == 0 {
		t.Fatalf("empty explain output:\n%s", raw)
	}
	return top[0].Plan.flatten()
}

// TestPlan_ListFlowsNameSort_WalksTheNameIndex: with sorting priced out, the
// library's name sort is served by an ordered walk of
// flows_lower_name_updated_idx (v29) with the semi-join and policy as filters —
// no Sort node. (The walk is unkeyed by design: the index delivers the order.)
func TestPlan_ListFlowsNameSort_WalksTheNameIndex(t *testing.T) {
	b := openMigrationTestDB(t)
	seedPlanPopulation(t, b)
	all := libraryPlanFilter(planUser)
	all.SortBy = interfaces.FlowSortNameAsc
	for _, tc := range []struct {
		name   string
		filter interfaces.FlowFilter
	}{
		{"scope all", all},
		{"scope mine", interfaces.FlowFilter{UserID: planUser, MetadataOnly: true, Limit: 25, SortBy: interfaces.FlowSortNameAsc}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, args := listFlowsSQL(tc.filter)
			nodes := explainAsUserWith(t, b, planUser, q, args, `SET LOCAL enable_sort = off`)
			t.Logf("ListFlows name sort (%s) plan:\n%s", tc.name, describePlan(nodes))
			assertIndexUsed(t, nodes, "flows_lower_name_updated_idx", "ListFlows name sort "+tc.name)
			assertNoSeqScan(t, nodes, "flows", "ListFlows name sort "+tc.name)
			for _, n := range nodes {
				if strings.Contains(n.NodeType, "Sort") {
					t.Errorf("ListFlows name sort (%s) still sorts (%s); the index must deliver the list's order:\n%s", tc.name, n.NodeType, describePlan(nodes))
				}
			}
			scan, ok := nodeOn(nodes, "flows")
			if !ok || scan.IndexName != "flows_lower_name_updated_idx" || scan.NodeType != "Index Scan" {
				t.Errorf("flows is reached by %s using %q, want an ordered Index Scan on flows_lower_name_updated_idx:\n%s", scan.NodeType, scan.IndexName, describePlan(nodes))
			}
		})
	}
}

// TestPlan_ListFlowsNameSortDesc_StillSorts proves the check observes the
// planner: FlowSortNameDesc is neither the index's order nor its reverse, so
// the plan carries a Sort node even with sorting priced out.
func TestPlan_ListFlowsNameSortDesc_StillSorts(t *testing.T) {
	b := openMigrationTestDB(t)
	seedPlanPopulation(t, b)
	f := libraryPlanFilter(planUser)
	f.SortBy = interfaces.FlowSortNameDesc
	q, args := listFlowsSQL(f)
	nodes := explainAsUserWith(t, b, planUser, q, args, `SET LOCAL enable_sort = off`)
	for _, n := range nodes {
		if strings.Contains(n.NodeType, "Sort") {
			return
		}
	}
	t.Fatalf("the descending name sort no longer sorts; the ascending plan test cannot tell an index walk from a sort:\n%s", describePlan(nodes))
}
