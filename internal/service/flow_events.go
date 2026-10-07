package service

import (
	"cmp"
	"context"
	"fmt"
	"os"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/eventbus"
	"pad-core/analyzer"
	"pad-core/models"
)

// FlowOption configures a FlowService at construction.
type FlowOption func(*FlowService)

// WithFlowEvents makes the service announce every flow it changes as events.FlowChanged on bus.
func WithFlowEvents(bus *eventbus.Bus) FlowOption {
	return func(s *FlowService) { s.bus = bus }
}

// publishDesktopChange announces that a desktop flow's files were rewritten: no store, so no version and no
// transaction.
func (s *FlowService) publishDesktopChange(ctx context.Context, doc *models.FlowDocument) {
	s.bus.Publish(ctx, events.FlowChanged{
		FlowID: doc.ID, StableID: analyzer.StableFlowID(doc), OwnerID: doc.OwnerID, OrgID: doc.OrganizationID,
		Kind: events.FlowUpdated,
	})
}

// ReimportFlow re-reads a desktop flow's source (doc.FilePath, a file or a folder) after it changed outside the
// app and returns the fresh document. The flow's report drops from the session analytics; the rest of the session
// survives.
func (s *FlowService) ReimportFlow(ctx context.Context, doc *models.FlowDocument) (*models.FlowDocument, error) {
	info, err := os.Stat(doc.FilePath)
	if err != nil {
		return nil, fmt.Errorf("source file not accessible: %w", err)
	}
	metrics.RecordFlowOp("reimport")
	var fresh *models.FlowDocument
	if info.IsDir() {
		fresh, err = s.LoadFlowFolder(doc.FilePath)
	} else {
		fresh, err = s.LoadFlowFromPath(doc.FilePath)
	}
	if err != nil {
		return nil, err
	}
	analyzer.DefaultCache.Invalidate(analyzer.StableFlowID(fresh))
	s.publishDesktopChange(ctx, fresh)
	return fresh, nil
}

// FlowCacheSubscriber is a sync subscriber of FlowChanged that keeps one flow-derived cache coherent.
type FlowCacheSubscriber struct {
	Name string
	Fn   func(context.Context, events.FlowChanged)
}

// FlowCacheSubscribers lists the caches derived from flows, for internal/di to subscribe on the sync tier: the
// owner's dashboard (and the analytics views spanning the flow's organisation), the chat's scrubbed context, the
// search index, and, once the flow is gone, its resolved document and its analyzer reports. A sharing change
// touches none of them (they do not depend on who may see the flow).
func FlowCacheSubscribers(flows *FlowService, dashboard *DashboardService, chat *ChatService) []FlowCacheSubscriber {
	content := func(fn func(events.FlowChanged)) func(context.Context, events.FlowChanged) {
		return func(_ context.Context, e events.FlowChanged) {
			if e.Kind != events.FlowSharingChanged {
				fn(e)
			}
		}
	}
	deleted := func(fn func(events.FlowChanged)) func(context.Context, events.FlowChanged) {
		return func(_ context.Context, e events.FlowChanged) {
			if e.Kind == events.FlowDeleted {
				fn(e)
			}
		}
	}
	return []FlowCacheSubscriber{
		{"dashboard-cache", content(func(e events.FlowChanged) { dashboard.InvalidateFlowViews(e.OwnerID, e.OrgID) })},
		{"chat-context-cache", content(func(e events.FlowChanged) { chat.InvalidateChatContext(e.FlowID) })},
		{"flow-search-index", content(func(e events.FlowChanged) { flows.InvalidateSearchIndex(e.FlowID) })},
		// Not required for correctness (the cache checks the stored version on every read), but a row that
		// reappears under the same id must not be served the deleted one's parse.
		{"flow-document-cache", deleted(func(e events.FlowChanged) {
			if p, ok := flows.DocProvider().(interface{ Invalidate(string) }); ok {
				p.Invalidate(e.FlowID)
			}
		})},
		{"analyzer-report-cache", deleted(func(e events.FlowChanged) {
			analyzer.DefaultCache.Invalidate(cmp.Or(e.StableID, e.FlowID))
		})},
	}
}
