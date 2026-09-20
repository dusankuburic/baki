package service

import (
	"context"
	"fmt"
	"sort"

	"pad-core/models"

	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// maxPortfolioFlows caps how many accessible flows the portfolio ranks in one
// pass. Ranking is in-memory (worst-health-first across the whole set), so the
// cap bounds the work; org fleets in the hundreds fit comfortably.
const maxPortfolioFlows = 1000

// PortfolioSort orders the portfolio's entries. The rollup totals always cover
// every ranked flow; sort and limit only shape the entries list, so "the 20
// riskiest flows" still reports the fleet's real average health.
type PortfolioSort string

const (
	// PortfolioSortRisk is the default: worst health first, then most errors.
	PortfolioSortRisk PortfolioSort = "risk"
	// PortfolioSortHealth is best health first.
	PortfolioSortHealth PortfolioSort = "health"
	// PortfolioSortErrors is most open errors first, then most warnings.
	PortfolioSortErrors PortfolioSort = "errors"
	// PortfolioSortName is flow name A to Z.
	PortfolioSortName PortfolioSort = "name"
	// PortfolioSortAnalyzed is most recently analyzed first.
	PortfolioSortAnalyzed PortfolioSort = "analyzed"
)

// ParsePortfolioSort maps the public ?sort= value to a PortfolioSort. Empty
// means the default risk ranking. Unknown values are rejected rather than
// silently ranked by risk, because a caller asking for an order it does not
// get would read the wrong flows as "top".
func ParsePortfolioSort(raw string) (PortfolioSort, error) {
	switch PortfolioSort(raw) {
	case "":
		return PortfolioSortRisk, nil
	case PortfolioSortRisk, PortfolioSortHealth, PortfolioSortErrors, PortfolioSortName, PortfolioSortAnalyzed:
		return PortfolioSort(raw), nil
	}
	return "", fmt.Errorf("%w: sort must be one of risk, health, errors, name, analyzed", ErrInvalidInput)
}

// BuildPortfolio assembles the org-wide governance portfolio for the caller:
// every flow they can access (optionally filtered to one org), ordered by
// sortBy, with each flow's latest persisted health. limit trims the entries
// (<= 0 or above the ranking cap means all ranked flows). Two queries (list +
// batch health), never N+1.
func (s *LibraryService) BuildPortfolio(ctx context.Context, userID, orgID string, sortBy PortfolioSort, limit int) (*models.Portfolio, error) {
	// Desktop/local mode has no storage backend: ListLibraryFlows already
	// returns an empty list there, but the health batch below dereferenced
	// s.storage regardless, so GET /api/library/portfolio panicked into a 500
	// on every desktop hit (the home KPI tile routed there). Same guard as
	// FlowHealth: an empty portfolio is the honest answer, not an error.
	if s.mode == config.ModeLocal || s.storage == nil {
		return &models.Portfolio{Entries: []models.PortfolioEntry{}}, nil
	}
	docs, err := s.ListLibraryFlows(ctx, userID, orgID, ScopeAll, "", "", maxPortfolioFlows, 0)
	if err != nil {
		return nil, err
	}
	// The real fleet size, not the size of the capped page: TotalFlows came
	// from len(docs), so a fleet larger than the ranking cap reported exactly
	// the cap and averaged over that slice as if it were everything.
	totalAccessible, err := s.CountLibraryFlows(ctx, userID, orgID, ScopeAll, "")
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(docs))
	ownerSet := make(map[string]struct{})
	for _, d := range docs {
		ids = append(ids, d.ID)
		if d.OwnerID != "" {
			ownerSet[d.OwnerID] = struct{}{}
		}
	}

	health, err := s.storage.LoadFlowHealthBatch(ctx, ids)
	if err != nil {
		return nil, err
	}

	ownerIDs := make([]string, 0, len(ownerSet))
	for id := range ownerSet {
		ownerIDs = append(ownerIDs, id)
	}
	names := s.ResolveOwnerNames(ctx, ownerIDs)

	return assemblePortfolio(docs, health, names, sortBy, limit, totalAccessible), nil
}

// assemblePortfolio is the pure assembly + ranking core (no I/O) so it can be
// unit-tested directly. Rollups are computed over every ranked doc before the
// entries are ordered and trimmed. totalAccessible is the caller's whole
// fleet (which may exceed the ranking cap); pass <= 0 to report the ranked
// count as the total. Every order except name puts unanalyzed flows last
// (they have no health to rank by); ties break by name.
func assemblePortfolio(docs []*storageif.FlowDocument, health map[string]*storageif.HealthSnapshot, ownerNames map[string]string, sortBy PortfolioSort, limit int, totalAccessible int) *models.Portfolio {
	p := &models.Portfolio{Entries: make([]models.PortfolioEntry, 0, len(docs))}
	for _, d := range docs {
		e := models.PortfolioEntry{
			FlowID:    d.ID,
			FlowName:  d.Name,
			OwnerID:   d.OwnerID,
			OwnerName: ownerNames[d.OwnerID],
		}
		if h := health[d.ID]; h != nil {
			e.Analyzed = true
			e.HealthScore = h.HealthScore
			e.Errors = h.Errors
			e.Warnings = h.Warnings
			e.Info = h.Info
			at := h.AnalyzedAt
			e.AnalyzedAt = &at
			p.AnalyzedFlows++
			p.AvgHealth += h.HealthScore
			p.Errors += h.Errors
			p.Warnings += h.Warnings
			p.Info += h.Info
		}
		p.Entries = append(p.Entries, e)
	}
	p.RankedFlows = len(docs)
	p.TotalFlows = len(docs)
	if totalAccessible > len(docs) {
		p.TotalFlows = totalAccessible
		p.Truncated = true
	}
	if p.AnalyzedFlows > 0 {
		p.AvgHealth /= p.AnalyzedFlows
	}

	sort.SliceStable(p.Entries, func(i, j int) bool {
		return portfolioLess(p.Entries[i], p.Entries[j], sortBy)
	})
	if limit > 0 && limit < len(p.Entries) {
		p.Entries = p.Entries[:limit]
	}
	return p
}

func portfolioLess(a, b models.PortfolioEntry, sortBy PortfolioSort) bool {
	if sortBy == PortfolioSortName {
		return a.FlowName < b.FlowName
	}
	if a.Analyzed != b.Analyzed {
		return a.Analyzed // analyzed flows rank above never-analyzed ones
	}
	if a.Analyzed {
		switch sortBy {
		case PortfolioSortHealth:
			if a.HealthScore != b.HealthScore {
				return a.HealthScore > b.HealthScore
			}
		case PortfolioSortErrors:
			if a.Errors != b.Errors {
				return a.Errors > b.Errors
			}
			if a.Warnings != b.Warnings {
				return a.Warnings > b.Warnings
			}
		case PortfolioSortAnalyzed:
			if at, bt := a.AnalyzedAt, b.AnalyzedAt; at != nil && bt != nil && !at.Equal(*bt) {
				return at.After(*bt)
			}
		default: // risk: worst health first, then most errors
			if a.HealthScore != b.HealthScore {
				return a.HealthScore < b.HealthScore
			}
			if a.Errors != b.Errors {
				return a.Errors > b.Errors
			}
		}
	}
	return a.FlowName < b.FlowName
}
