package service

import (
	"context"
	"sort"

	"pad-core/models"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// maxPortfolioFlows caps how many accessible flows the portfolio ranks in one
// pass. Ranking is in-memory (worst-health-first across the whole set), so the
// cap bounds the work; org fleets in the hundreds fit comfortably.
const maxPortfolioFlows = 1000

// BuildPortfolio assembles the org-wide governance portfolio for the caller:
// every flow they can access (optionally filtered to one org), ranked by risk,
// with each flow's latest persisted health. Two queries (list + batch health),
// never N+1.
func (s *LibraryService) BuildPortfolio(ctx context.Context, userID, orgID string) (*models.Portfolio, error) {
	docs, err := s.ListLibraryFlows(ctx, userID, orgID, ScopeAll, "", "", maxPortfolioFlows, 0)
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

	return assemblePortfolio(docs, health, names), nil
}

// assemblePortfolio is the pure assembly + ranking core (no I/O) so it can be
// unit-tested directly. Flows are ranked worst-health-first; unanalyzed flows
// sort last, then ties break by error count and name.
func assemblePortfolio(docs []*storageif.FlowDocument, health map[string]*storageif.HealthSnapshot, ownerNames map[string]string) *models.Portfolio {
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
	p.TotalFlows = len(docs)
	if p.AnalyzedFlows > 0 {
		p.AvgHealth /= p.AnalyzedFlows
	}

	sort.SliceStable(p.Entries, func(i, j int) bool {
		a, b := p.Entries[i], p.Entries[j]
		if a.Analyzed != b.Analyzed {
			return a.Analyzed // analyzed flows rank above never-analyzed ones
		}
		if a.Analyzed { // both analyzed: worst health first, then most errors
			if a.HealthScore != b.HealthScore {
				return a.HealthScore < b.HealthScore
			}
			if a.Errors != b.Errors {
				return a.Errors > b.Errors
			}
		}
		return a.FlowName < b.FlowName
	})
	return p
}
