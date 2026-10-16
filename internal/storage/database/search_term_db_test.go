package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/google/uuid"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil/pgtest"
)

// TestSearchWordChar_CountsEveryWordCharacterOfTheDatabase compares interfaces.SearchWordChar with pg_trgm's own
// word characters (show_trgm of one character is empty for any other) over every code point, on the test
// database: a character the database counts and Go's tables know as letters or digits (L*/Nd/Nl) must count to Go too,
// or a word the index serves is refused (the Indic, Thai and Arabic vowel signs once were).
//
// Note on environments: CI and compose use pgvector:pg16 based on Debian Bookworm (glibc 2.36, Unicode 15.0).
// Newer images like pg16-trixie and official postgres:16 use glibc >= 2.41 (Unicode 16.0), where thousands of
// characters have been newly given Alphabetic properties; non-L*/Nd/Nl code points (Mn/Mc/So/Cn) that the database
// counts are logged rather than failed.
func TestSearchWordChar_CountsEveryWordCharacterOfTheDatabase(t *testing.T) {
	b := openMigrationTestDB(t)
	rows, err := b.db.QueryContext(context.Background(), `SELECT cp FROM generate_series(1, 1114111) cp
		WHERE cp NOT BETWEEN 55296 AND 57343 AND show_trgm(chr(cp)) <> '{}'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	words := map[rune]bool{}
	for rows.Next() {
		var cp int32
		if err := rows.Scan(&cp); err != nil {
			t.Fatal(err)
		}
		words[cp] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var missed []string
	var newerTables []string
	goOnly := 0
	for r := rune(1); r <= unicode.MaxRune; r++ {
		isDbWord := words[r]
		isGoWord := interfaces.SearchWordChar(r)
		switch {
		case isDbWord && !isGoWord:
			if !unicode.Is(unicode.Cn, r) && (unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Nl, r)) {
				missed = append(missed, fmt.Sprintf("U+%04X %q", r, r))
			} else {
				newerTables = append(newerTables, fmt.Sprintf("U+%04X %q", r, r))
			}
		case !isDbWord && isGoWord:
			goOnly++
		}
	}
	if len(missed) > 0 {
		t.Errorf("%d word characters of the database are none to SearchWordChar (a word with them is refused), e.g. %v",
			len(missed), missed[:min(len(missed), 20)])
	}
	if len(newerTables) > 0 {
		t.Logf("%d database word characters belong to newer Unicode tables/marks (Mn/Mc/So/Cn in Go), e.g. %v",
			len(newerTables), newerTables[:min(len(newerTables), 10)])
	}
	t.Logf("%d database word characters; %d more to Go (Unicode %s), which the store asks the database about", len(words), goOnly, unicode.Version)
}

// TestSearchTerms_TheDatabaseDecides: a term Go calls long but the database draws no trigram from (letters newer
// than its C library's tables, here Unicode 15's) is refused by the content search and matched as a prefix by the
// name filters, as a short term is; nothing reads flow_search with it. A word whose vowel signs the database counts
// searches by substring through the index.
func TestSearchTerms_TheDatabaseDecides(t *testing.T) {
	b := openMigrationTestDB(t)
	run := uuid.NewString()[:8]
	newer := "\U00031350\U00031351\U00031352" // CJK Ext H, Unicode 15
	checkTermsServed(t, b, run, newer, "नाम", dbDrawsATrigram(t, b, newer))
	checkTermsServed(t, b, run+"-w", "नाम", "abc", true)
}

// TestSearchTerms_UnderACCtype: in a database whose LC_CTYPE is C, pg_trgm counts ASCII letters and digits only, so
// every other script's words are short: refused by the content search, a prefix in the name filters, and never a
// flow_search read. ASCII words search as everywhere.
func TestSearchTerms_UnderACCtype(t *testing.T) {
	dsn := newCCtypeDatabase(t)
	b, err := New(context.Background(), DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("migrate the C-ctype database: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	run := uuid.NewString()[:8]
	for i, term := range []string{"日本語", "абв", "नाम"} {
		if dbDrawsATrigram(t, b, term) {
			t.Fatalf("%q draws a trigram under the C ctype; the database is not what the test needs", term)
		}
		checkTermsServed(t, b, fmt.Sprintf("%s-%d", run, i), term, "abc", false)
	}
}

// dbDrawsATrigram reports whether pg_trgm finds a run of three word characters in term in b's database.
func dbDrawsATrigram(t *testing.T, b *PostgresStorageBackend, term string) bool {
	t.Helper()
	var ok bool
	if err := b.db.QueryRowContext(context.Background(), `SELECT $1::text ~ '[[:alnum:]]{3}'`, term).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// checkTermsServed seeds flows and users that contain term at their start and inside, and checks every search
// path: when served (the database draws a trigram from term) a substring, else a prefix (names) or a refusal
// (content). word must always search by substring.
func checkTermsServed(t *testing.T, b *PostgresStorageBackend, run, term, word string, served bool) {
	t.Helper()
	ctx := context.Background()
	owner := "dbterm-owner-" + run
	id := func(n string) string { return "dbterm-" + run + "-" + n }
	for _, f := range []*interfaces.FlowDocument{
		{ID: id("starts"), Name: term + " report", OwnerID: owner},
		{ID: id("inside"), Name: "report " + term, OwnerID: owner},
	} {
		f.Content = json.RawMessage(`{"source":"Display.ShowMessage Message: '` + term + ` ` + word + `'"}`)
		if err := b.SaveFlow(ctx, f); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}
		fid := f.ID
		t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), fid) })
	}
	for _, u := range []*interfaces.User{
		{ID: id("u-starts"), Email: id("u-starts") + "@example.com", Password: "x", DisplayName: term + " ops"},
		{ID: id("u-inside"), Email: id("u-inside") + "@example.com", Password: "x", DisplayName: "ops " + term},
	} {
		if err := b.SaveUser(ctx, u); err != nil {
			t.Fatalf("SaveUser: %v", err)
		}
		uid := u.ID
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), uid) })
	}
	both := []string{id("inside"), id("starts")}
	want, wantUsers := both, []string{id("u-inside"), id("u-starts")}
	if !served {
		want, wantUsers = []string{id("starts")}, []string{id("u-starts")}
	}

	filter := interfaces.FlowFilter{UserID: owner}
	hits, err := b.SearchFlowContents(ctx, filter, term, 0)
	switch got := flowIDs(hits); {
	case served && (err != nil || !slices.Equal(sorted(got), both)):
		t.Errorf("SearchFlowContents(%q) = %v, %v; want both flows", term, got, err)
	case !served && !errors.Is(err, interfaces.ErrQueryTooShort):
		t.Errorf("SearchFlowContents(%q) = %v, %v; want ErrQueryTooShort (no trigram in this database)", term, got, err)
	}
	if hits, err := b.SearchFlowContents(ctx, filter, word, 0); err != nil || !slices.Equal(sorted(flowIDs(hits)), both) {
		t.Errorf("SearchFlowContents(%q) = %v, %v; want both flows", word, flowIDs(hits), err)
	}

	filter.Query = term
	flows, err := b.ListFlows(ctx, filter)
	if err != nil || !slices.Equal(sorted(flowIDs(flows)), want) {
		t.Errorf("ListFlows(%q) = %v, %v; want %v", term, flowIDs(flows), err, want)
	}
	if n, err := b.CountFlows(ctx, filter); err != nil || n != len(want) {
		t.Errorf("CountFlows(%q) = %d, %v; want %d", term, n, err, len(want))
	}
	if page, err := b.ListFlowsPage(ctx, filter, 0); err != nil || page.Total != len(want) || !slices.Equal(sorted(flowIDs(page.Docs)), want) {
		t.Errorf("ListFlowsPage(%q) = %+v, %v; want %v", term, page, err, want)
	}

	// The statements ListFlows and CountFlows run: the substring through flow_search for a term its index serves,
	// else the prefix on the caller's own flows, never flow_search.
	resolved, err := b.nameQuery(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	for what, stmt := range map[string]func(interfaces.FlowFilter) (string, []any){"ListFlows": listFlowsSQL, "CountFlows": countFlowsSQL} {
		q, args := stmt(resolved)
		nodes := explainAsUser(t, b, owner, q, args)
		reads := slices.ContainsFunc(nodes, func(n planNode) bool { return n.RelationName == "flow_search" })
		if reads != served {
			t.Errorf("%s name query %q reads flow_search: %v, want %v:\n%s", what, term, reads, served, describePlan(nodes))
		}
	}

	users, total, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: term, Limit: 500})
	var got []string
	for _, u := range users {
		if strings.HasPrefix(u.ID, id("u-")) {
			got = append(got, u.ID)
		}
	}
	if err != nil || !slices.Equal(sorted(got), wantUsers) || total != len(wantUsers) {
		t.Errorf("QueryUsers(%q) = %v of %d, %v; want %v", term, got, total, err, wantUsers)
	}
}

func sorted(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

// newCCtypeDatabase creates an empty database whose collation and ctype are C on the DATABASE_URL server, dropped
// when the test ends, and returns its DSN. It is cloned from template0 (another ctype than the template's needs
// it), so it has no vector extension; the migrations do without.
func newCCtypeDatabase(t *testing.T) string {
	t.Helper()
	dsn := pgtest.DSN(t)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "baki_t_cctype_" + strings.ReplaceAll(uuid.NewString()[:13], "-", "")
	if _, err := admin.ExecContext(context.Background(),
		`CREATE DATABASE `+name+` TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'`); err != nil {
		t.Fatalf("create a C-ctype database: %v", err)
	}
	t.Cleanup(func() {
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer admin.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
