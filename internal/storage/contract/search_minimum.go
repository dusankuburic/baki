package contract

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// runSearchMinimumScenarios holds the shortest search terms: a trigram index serves no term without a run of three
// letters or digits (under three characters, or punctuation like "---"), so content search refuses one
// (ErrQueryTooShort) and the library's name filter matches a prefix of the name instead of a substring, on the
// caller's own flows.
func runSearchMinimumScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	saveFlow := func(t *testing.T, f *interfaces.FlowDocument) {
		t.Helper()
		if f.Content == nil {
			f.Content = json.RawMessage(`{}`)
		}
		if err := b.SaveFlow(ctx, f); err != nil {
			t.Fatalf("SaveFlow %s: %v", f.ID, err)
		}
		id := f.ID
		t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), id) })
	}

	run("SearchFlowContents_refuses_a_term_under_three_characters", func(t *testing.T) {
		owner := "contract-smin-owner-" + runID
		saveFlow(t, &interfaces.FlowDocument{ID: "contract-smin-" + runID, Name: "Short", OwnerID: owner,
			Content: json.RawMessage(`{"source":"Display.ShowMessage Message: 'abc 日本語'"}`)})
		filter := interfaces.FlowFilter{UserID: owner}
		if _, err := b.SearchFlowContents(ctx, filter, "abc", 0); errors.Is(err, interfaces.ErrContentSearchUnsupported) {
			t.Skip("backend has no content search")
		}
		// Three characters or more without a run of three letters or digits draw no trigram either.
		for _, term := range []string{"a", "ab", "  ab  ", "日本", "---", "...", "%%%", "___", "a b", "a-b-c"} {
			if hits, err := b.SearchFlowContents(ctx, filter, term, 0); !errors.Is(err, interfaces.ErrQueryTooShort) {
				t.Errorf("SearchFlowContents(%q) = %v, %v; want ErrQueryTooShort", term, flowIDs(hits), err)
			}
		}
		// Three characters, counted as characters rather than bytes, search as before.
		for _, term := range []string{"abc", "日本語"} {
			hits, err := b.SearchFlowContents(ctx, filter, term, 0)
			if err != nil || !slices.Equal(flowIDs(hits), []string{"contract-smin-" + runID}) {
				t.Errorf("SearchFlowContents(%q) = %v, %v; want the flow", term, flowIDs(hits), err)
			}
		}
	})

	run("ListFlows_name_query_under_three_characters_matches_a_prefix", func(t *testing.T) {
		owner, other := "contract-snp-owner-"+runID, "contract-snp-other-"+runID
		id := func(n string) string { return "contract-snp-" + runID + "-" + n }
		for _, f := range []*interfaces.FlowDocument{
			{ID: id("abacus"), Name: "Abacus report", OwnerID: owner},
			{ID: id("cab"), Name: "Cab ride", OwnerID: owner},
			{ID: id("underscore"), Name: "a_b flow", OwnerID: owner},
			{ID: id("axb"), Name: "axb flow", OwnerID: owner},
			{ID: id("dashes"), Name: "--- draft", OwnerID: owner},
			// Another tenant's flow with a matching name: never in the owner's results.
			{ID: id("theirs"), Name: "Abba", OwnerID: other},
			{ID: id("their-dashes"), Name: "--- theirs", OwnerID: other},
		} {
			saveFlow(t, f)
		}
		all := []string{id("abacus"), id("axb"), id("cab"), id("dashes"), id("underscore")}
		for _, tc := range []struct {
			query string
			want  []string
		}{
			// Under three characters (after trimming): a case-insensitive prefix of the name.
			{"ab", []string{id("abacus")}},
			{"AB", []string{id("abacus")}},
			{" ab ", []string{id("abacus")}},
			{"a", []string{id("abacus"), id("axb"), id("underscore")}},
			// "_" is a character, not a wildcard.
			{"a_", []string{id("underscore")}},
			// Blank: no name filter.
			{"   ", all},
			// No run of three letters or digits, however long: a prefix too.
			{"---", []string{id("dashes")}},
			{"b f", nil},
			// A run of three or more: a substring, as before.
			{"cab", []string{id("cab")}},
			{"flo", []string{id("axb"), id("underscore")}},
		} {
			filter := interfaces.FlowFilter{UserID: owner, Query: tc.query}
			flows, err := b.ListFlows(ctx, filter)
			if err != nil {
				t.Fatalf("ListFlows(%q): %v", tc.query, err)
			}
			got := flowIDs(flows)
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("ListFlows(%q) = %v, want %v", tc.query, got, want)
			}
			if n, err := b.CountFlows(ctx, filter); err != nil || n != len(want) {
				t.Errorf("CountFlows(%q) = %d, %v; want %d", tc.query, n, err, len(want))
			}
		}
	})

	// A word's vowel signs and viramas are word characters to pg_trgm (glibc's iswalnum, a UTF-8 ctype as every
	// shipped database has), so these words search; a tone mark or a combining accent splits a word for it too.
	run("SearchFlowContents_searches_words_with_vowel_signs", func(t *testing.T) {
		owner := "contract-sword-owner-" + runID
		id := "contract-sword-" + runID
		words := []string{"नाम", "त्रुटि", "বার্তা", "கோப்பு", "สวัสดี", "مُدِير"}
		saveFlow(t, &interfaces.FlowDocument{ID: id, Name: "Words", OwnerID: owner,
			Content: json.RawMessage(`{"source":"Display.ShowMessage Message: '` + strings.Join(words, " ") + ` ชื่อ e\u0301te\u0301'"}`)})
		filter := interfaces.FlowFilter{UserID: owner}
		if _, err := b.SearchFlowContents(ctx, filter, "abc", 0); errors.Is(err, interfaces.ErrContentSearchUnsupported) {
			t.Skip("backend has no content search")
		}
		for _, term := range words {
			if hits, err := b.SearchFlowContents(ctx, filter, term, 0); err != nil || !slices.Equal(flowIDs(hits), []string{id}) {
				t.Errorf("SearchFlowContents(%q) = %v, %v; want the flow", term, flowIDs(hits), err)
			}
		}
		for _, term := range []string{"ชื่อ", "e\u0301te\u0301"} {
			if hits, err := b.SearchFlowContents(ctx, filter, term, 0); !errors.Is(err, interfaces.ErrQueryTooShort) {
				t.Errorf("SearchFlowContents(%q) = %v, %v; want ErrQueryTooShort", term, flowIDs(hits), err)
			}
		}
	})

	// QueryPrefix and UserQuery.Prefix ask for the prefix match whatever the term: the Postgres store sets them for
	// a term its database draws no trigram from.
	run("Prefix_flags_match_a_prefix_whatever_the_term", func(t *testing.T) {
		owner := "contract-spf-owner-" + runID
		id := func(n string) string { return "contract-spf-" + runID + "-" + n }
		saveFlow(t, &interfaces.FlowDocument{ID: id("starts"), Name: "Gamma report", OwnerID: owner})
		saveFlow(t, &interfaces.FlowDocument{ID: id("inside"), Name: "Report gamma", OwnerID: owner})
		for _, tc := range []struct {
			prefix bool
			want   []string
		}{{false, []string{id("inside"), id("starts")}}, {true, []string{id("starts")}}} {
			filter := interfaces.FlowFilter{UserID: owner, Query: "GAMMA", QueryPrefix: tc.prefix}
			flows, err := b.ListFlows(ctx, filter)
			got := flowIDs(flows)
			slices.Sort(got)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("ListFlows(QueryPrefix %v) = %v, %v; want %v", tc.prefix, got, err, tc.want)
			}
			if n, err := b.CountFlows(ctx, filter); err != nil || n != len(tc.want) {
				t.Errorf("CountFlows(QueryPrefix %v) = %d, %v; want %d", tc.prefix, n, err, len(tc.want))
			}
			if page, err := b.ListFlowsPage(ctx, filter, 0); err != nil || len(page.Docs) != len(tc.want) || page.Total != len(tc.want) {
				t.Errorf("ListFlowsPage(QueryPrefix %v) = %+v, %v; want %d flows", tc.prefix, page, err, len(tc.want))
			}
		}

		uid := "contract-spu-" + runID
		if err := b.SaveUser(ctx, &interfaces.User{ID: uid, Email: uid + "@example.com", Password: "hash", DisplayName: "Ops " + runID}); err != nil {
			t.Fatalf("SaveUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), uid) })
		for _, tc := range []struct {
			q      string
			prefix bool
			found  bool
		}{{runID, false, true}, {runID, true, false}, {"ops " + runID, true, true}, {uid, true, true}} {
			users, _, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: tc.q, Prefix: tc.prefix, Limit: 500})
			found := slices.ContainsFunc(users, func(u *interfaces.User) bool { return u.ID == uid })
			if err != nil || found != tc.found {
				t.Errorf("QueryUsers(%q, Prefix %v) finds the user: %v, %v; want %v", tc.q, tc.prefix, found, err, tc.found)
			}
		}
	})
}
