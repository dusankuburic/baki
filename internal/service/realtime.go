package service

import (
	"context"
	"time"

	"pad-analyzer/internal/events"
)

// The realtime subscribers: async (internal/di subscribes them), so a client told to refetch reads the committed
// write, and a rolled-back one tells no one.

// NotifySettingsChanged returns the subscriber that tells a scope's connected clients their settings changed
// (settings:changed; the client re-reads them, the payload only names the scope): a user's own clients, every member
// of an organisation, or for the deployment the clients of a deployment without accounts (LocalUser). With accounts
// a deployment change reaches no one, as it never did. Not subscribed while SystemService emits settings:changed
// itself with the saved settings: SettingsChanged carries no payload, and is also published by changes that send
// no settings:changed today (org custom rules, deployment rule toggles).
func NotifySettingsChanged(n EventNotifier) func(context.Context, events.SettingsChanged) {
	return func(ctx context.Context, e events.SettingsChanged) {
		data := map[string]string{"scope": string(e.Scope)}
		switch e.Scope {
		case events.UserSettings:
			if e.ID != "" {
				n.EmitTo(e.ID, "settings:changed", data)
			}
		case events.OrgSettings:
			n.EmitToOrg(ctx, e.ID, "settings:changed", data)
		case events.DeploymentSettings:
			n.EmitTo(LocalUser, "settings:changed", data)
		}
	}
}

// FlowViewers is the WebSocket hub as the flow subscriber sees it: the clients that have a flow open.
type FlowViewers interface {
	// NotifyFlowChanged tells the flow's viewers to refetch it; a viewer skips a version equal to the one it last
	// applied.
	NotifyFlowChanged(flowID string, version int)
}

// NotifyFlowViewers returns the subscriber that sends flow.changed to a flow's open viewers when its content
// changed. Only updates notify: a new flow has no viewers yet, a deleted one has nothing to refetch (the reload
// would fail), and a sharing change leaves the content as it was.
//
// The version sent is the time in milliseconds, never the stored version: a viewer records as applied the version
// of the metadata it read, which its reload fetches in parallel with the content, so a reload that read the content
// just before the commit and the metadata just after it holds the old content under the new version. A notification
// carrying the stored version would then be skipped as already applied and leave the viewer on the stale content
// (its next save would pass the version check and overwrite this change); a stamp never equals an applied version,
// so the viewer always refetches.
func NotifyFlowViewers(v FlowViewers) func(context.Context, events.FlowChanged) {
	return func(_ context.Context, e events.FlowChanged) {
		if e.Kind != events.FlowUpdated || e.FlowID == "" {
			return
		}
		v.NotifyFlowChanged(e.FlowID, int(time.Now().UnixMilli()))
	}
}
