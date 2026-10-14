package service

import (
	"cmp"
	"context"
	"fmt"
	"os"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-core/analyzer"
	"pad-core/cache"
	"pad-core/models"
)

// FlowOption configures a FlowService at construction.
type FlowOption func(*FlowService)

// WithFlowEvents makes the service announce every flow it changes as events.FlowChanged on bus.
func WithFlowEvents(bus *eventbus.Bus) FlowOption {
	return func(s *FlowService) { s.bus = bus }
}

// WithAnalyzer gives the service the process's cached analyzer, the one AnalysisService serves reports from, so the
// flow changes it makes reach those reports.
func WithAnalyzer(a *analyzer.CachedAnalyzer) FlowOption {
	return func(s *FlowService) { s.analyzer = a }
}

// ResetSessionAnalytics drops the local user's cached reports: opening a base folder on the desktop starts a new
// analytics session.
func (s *FlowService) ResetSessionAnalytics() {
	s.analyzer.ResetTenant(cache.Local())
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
	RecordFlowOp("reimport")
	var fresh *models.FlowDocument
	if info.IsDir() {
		fresh, err = s.LoadFlowFolder(doc.FilePath)
	} else {
		fresh, err = s.LoadFlowFromPath(doc.FilePath)
	}
	if err != nil {
		return nil, err
	}
	s.analyzer.InvalidateFlow(analyzer.StableFlowID(fresh))
	s.publishDesktopChange(ctx, fresh)
	return fresh, nil
}

// FlowCacheSubscriber is a sync subscriber of FlowChanged that keeps one flow-derived cache coherent.
type FlowCacheSubscriber struct {
	Name string
	Fn   func(context.Context, events.FlowChanged)
	// Options carry the subscriber's locality (eventbus.Local or eventbus.Replicated).
	Options []eventbus.SubscribeOption
}

// flowViewInvalidations is what the other replicas drop when a flow's content changes: the owner's home and
// analytics views and the analytics views spanning the flow's organisation (DashboardService.InvalidateFlowViews).
func flowViewInvalidations(e events.FlowChanged) []eventbus.Invalidation {
	if e.Kind == events.FlowSharingChanged {
		return nil
	}
	scope, ok := flowViewScope(e.OwnerID, e.OrgID)
	if !ok {
		return nil
	}
	return []eventbus.Invalidation{{Key: "dashboard.home", Scope: scope}, {Key: "dashboard.analytics", Scope: scope}}
}

// flowViewScope names the views a change to a flow of ownerID in orgID drops, as the scope of dashboard.home and
// dashboard.analytics; false when it names nobody (the local drop ignores that too, and an empty scope would mean
// every view).
func flowViewScope(ownerID, orgID string) (eventbus.Scope, bool) {
	switch {
	case ownerID != "" && orgID != "":
		return eventbus.NewScope("owner", ownerID, "org", orgID), true
	case ownerID != "":
		return eventbus.NewScope("owner", ownerID), true
	case orgID != "":
		return eventbus.NewScope("org", orgID), true
	}
	return nil, false
}

// ApplyRemoteViewInvalidation is the applier of dashboard.home and dashboard.analytics (Bus.OnRemote): the drop a
// peer's flow change or analysis made, for the owner and organisation its scope names. An empty scope (the
// receiver's resync after a gap) drops every view; a scope that names neither drops nothing.
func (s *DashboardService) ApplyRemoteViewInvalidation(scope eventbus.Scope) {
	if len(scope) == 0 {
		s.InvalidateDashboardCache("")
		return
	}
	s.InvalidateFlowViews(scope["owner"], scope["org"])
}

// WithDashboardEvents lets the service announce the drops that are not a FlowChanged's (RecordAnalysis) to the
// other replicas on bus.
func WithDashboardEvents(bus *eventbus.Bus) DashboardOption {
	return func(s *DashboardService) { s.bus = bus }
}

// SubscribeRemoteDashboard registers the applier of dashboard.home and dashboard.analytics: a peer's flow change or
// analysis drops the views it names here (ApplyRemoteViewInvalidation). One applier serves both view caches.
func SubscribeRemoteDashboard(bus *eventbus.Bus, d *DashboardService) {
	for _, key := range []string{"dashboard.analytics", "dashboard.home"} {
		bus.OnRemote(key, func(sc eventbus.Scope) { d.ApplyRemoteViewInvalidation(sc) })
	}
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
	local := func(reason string) []eventbus.SubscribeOption {
		return []eventbus.SubscribeOption{eventbus.Local(reason)}
	}
	return []FlowCacheSubscriber{
		{"dashboard-cache", content(func(e events.FlowChanged) { dashboard.InvalidateFlowViews(e.OwnerID, e.OrgID) }),
			[]eventbus.SubscribeOption{eventbus.Replicated(flowViewInvalidations, "dashboard.analytics", "dashboard.home")}},
		{"chat-context-cache", content(func(e events.FlowChanged) { chat.InvalidateChatContext(e.FlowID) }),
			local("keys carry the flow version, so another replica misses on a new version; the drop is hygiene")},
		{"flow-search-index", content(func(e events.FlowChanged) { flows.InvalidateSearchIndex(e.FlowID) }),
			local("served only for the exact document object it was built from, which each replica resolves through its own version-checked read")},
		// Not required for correctness (the cache checks the stored version on every read), but a row that
		// reappears under the same id must not be served the deleted one's parse.
		{"flow-document-cache", deleted(func(e events.FlowChanged) {
			if p, ok := flows.DocProvider().(interface{ Invalidate(string) }); ok {
				p.Invalidate(e.FlowID)
			}
		}), local("every read re-checks the stored version, so another replica's change or delete is seen at once")},
		{"analyzer-report-cache", deleted(func(e events.FlowChanged) {
			flows.analyzer.InvalidateFlow(cmp.Or(e.StableID, e.FlowID))
		}), local("a report is a function of the document, rule profile and analyzer version; another replica recomputes on a miss")},
	}
}
