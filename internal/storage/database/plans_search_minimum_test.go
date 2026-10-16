package database

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// shortTerms draw no trigram from a LIKE pattern: under three characters, or longer without a run of three letters or
// digits (pg_trgm splits words on everything else, so "---" is as empty as "ab").
var shortTerms = []string{"ab", "---", "...", "%%%", "___", "a b"}

// TestPlan_ListFlows_ShortNameQueryStaysOnTheCallersFlows: a short name query never reaches flow_search (no RLS, and
// with no trigram to serve the term it was read for every tenant) and reaches flows through the caller's visibility
// arms, as the library page without a query does, in every sort.
func TestPlan_ListFlows_ShortNameQueryStaysOnTheCallersFlows(t *testing.T) {
	b := openMigrationTestDB(t)
	seedPlanPopulation(t, b)
	for _, term := range shortTerms {
		for _, sort := range []interfaces.FlowSort{interfaces.FlowSortUpdatedDesc, interfaces.FlowSortNameAsc} {
			f := libraryPlanFilter(planUser)
			f.Query, f.SortBy = term, sort
			for what, stmt := range map[string]func(interfaces.FlowFilter) (string, []any){"ListFlows": listFlowsSQL, "CountFlows": countFlowsSQL} {
				q, args := stmt(f)
				nodes := explainAsUser(t, b, planUser, q, args)
				assertNoSeqScan(t, nodes, "flows", what+" short name query "+term)
				for _, n := range nodes {
					if n.RelationName == "flow_search" {
						t.Errorf("%s name query %q (sort %d) reads flow_search:\n%s", what, term, sort, describePlan(nodes))
						break
					}
				}
			}
		}
	}
}

// TestSearchFlowContents_RefusesATrigramlessTerm: the content search of a short term would read every tenant's
// flow_search.body (no trigram to probe the index with), so it is refused before any statement.
func TestSearchFlowContents_RefusesATrigramlessTerm(t *testing.T) {
	b := openMigrationTestDB(t)
	for _, term := range shortTerms {
		if hits, err := b.SearchFlowContents(context.Background(), libraryPlanFilter(planUser), term, 0); !errors.Is(err, interfaces.ErrQueryTooShort) {
			t.Errorf("SearchFlowContents(%q) = %d hits, %v; want ErrQueryTooShort", term, len(hits), err)
		}
	}
}
