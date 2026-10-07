package api

import (
	"context"

	"pad-analyzer/internal/api/route"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// Module is a feature's HTTP surface. Mount registers the feature's routes on the router's Builder and declares
// each route's policy beside it. Modules reach the router through fx's "routes" group (internal/di), so a new
// feature needs no edit to the router or to a central route list.
type Module interface {
	Mount(rb *route.Builder)
}

// flowNotifierSetter is a module whose writes tell a flow's live viewers that it changed; NewRouter hands it
// the WebSocket hub.
type flowNotifierSetter interface {
	SetFlowNotifier(n FlowNotifier)
}

// sharedFlowLookup resolves a share token to its flow's header for the /shared shell (serveSharedShell), or nil
// for an unknown token.
type sharedFlowLookup interface {
	LookupSharedFlowHeader(ctx context.Context, token string) *storageif.FlowDocument
}
