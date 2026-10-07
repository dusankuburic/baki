package service

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/testutil"
	"pad-core/parser"
)

// TestEditorMutations_InvalidateDerivedCaches is the R3.3 coherence gate:
// EVERY editor mutation path must announce the change (FlowChanged), which
// drops the derived caches (search index, the chat context's scrubbed-doc
// cache, the dashboard: FlowCacheSubscribers). Before R3.3, PatchFlow and
// persistCloudDoc never invalidated: a user could delete a block, then ask the
// AI about the flow and get answers describing the deleted block (cached
// pre-edit context).
func TestEditorMutations_InvalidateDerivedCaches(t *testing.T) {
	record := func(svc *FlowService) *int {
		bus := eventbus.New(nil, time.Second)
		svc.bus = bus
		fired := 0
		eventbus.Subscribe(bus, eventbus.Sync, "probe", func(_ context.Context, e events.FlowChanged) {
			if e.Kind != events.FlowSharingChanged {
				fired++
			}
		})
		return &fired
	}

	t.Run("desktop RemoveBlock", func(t *testing.T) {
		svc, doc := newDesktopEditSvc(t)
		fired := record(svc)
		target := blockIDByURL(doc, "https://target")
		if _, err := svc.RemoveBlock(context.Background(), doc, target); err != nil {
			t.Fatalf("RemoveBlock: %v", err)
		}
		if *fired != 1 {
			t.Errorf("invalidations = %d, want 1", *fired)
		}
	})

	t.Run("desktop DuplicateBlock", func(t *testing.T) {
		svc, doc := newDesktopEditSvc(t)
		fired := record(svc)
		target := blockIDByURL(doc, "https://target")
		if _, err := svc.DuplicateBlock(context.Background(), doc, target); err != nil {
			t.Fatalf("DuplicateBlock: %v", err)
		}
		if *fired != 1 {
			t.Errorf("invalidations = %d, want 1", *fired)
		}
	})

	t.Run("desktop UpdateBlockProperties", func(t *testing.T) {
		svc, doc := newDesktopEditSvc(t)
		fired := record(svc)
		target := blockIDByURL(doc, "https://target")
		if _, err := svc.UpdateBlockProperties(context.Background(), doc, target, map[string]string{"Url": "https://z"}); err != nil {
			t.Fatalf("UpdateBlockProperties: %v", err)
		}
		if *fired != 1 {
			t.Errorf("invalidations = %d, want 1", *fired)
		}
	})

	t.Run("desktop MoveBlock", func(t *testing.T) {
		svc, doc := newDesktopEditSvc(t)
		fired := record(svc)
		target := blockIDByURL(doc, "https://target")
		if _, err := svc.MoveBlock(context.Background(), doc, target, "up"); err != nil {
			t.Fatalf("MoveBlock: %v", err)
		}
		if *fired != 1 {
			t.Errorf("invalidations = %d, want 1", *fired)
		}
	})

	t.Run("desktop auto-fix rides PatchFlow too", func(t *testing.T) {
		svc, doc := newDesktopEditSvc(t)
		fired := record(svc)
		target := blockIDByURL(doc, "https://target")
		if _, err := svc.ApplyFix(context.Background(), doc, target, "wrap-error-handler", "unhandled-error", "", "", false); err != nil {
			t.Fatalf("ApplyFix: %v", err)
		}
		if *fired != 1 {
			t.Errorf("invalidations = %d, want 1", *fired)
		}
	})

	t.Run("cloud DuplicateBlock via persistCloudDoc", func(t *testing.T) {
		src := "#Region \"Main\"\n    HTTPClient.InvokeUrl Url: $'''https://c''' Method: HTTPClient.Method.GET\n#EndRegion\n"
		doc, err := parser.ParseText(src, "Cloudy", int64(len(src)))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		doc.Source = src
		doc.FilePath = ""
		doc.RebuildIndexes()
		ldp := NewLocalDocumentProvider()
		ldp.SetCurrentDoc(doc)
		svc := NewFlowService(&testutil.CountingNotifier{}, nil, ldp, &testutil.FakeBackend{}, nil, nil)
		fired := record(svc)
		if _, err := svc.DuplicateBlock(context.Background(), doc, httpURLBlockIDRecursive(doc, "https://c")); err != nil {
			t.Fatalf("DuplicateBlock(cloud): %v", err)
		}
		if *fired != 1 {
			t.Errorf("invalidations = %d, want 1", *fired)
		}
	})

	t.Run("desktop snapshot restore", func(t *testing.T) {
		svc, doc := newDesktopEditSvc(t)
		// Create a snapshot first (remove fires invalidation once).
		if _, err := svc.RemoveBlock(context.Background(), doc, blockIDByURL(doc, "https://target")); err != nil {
			t.Fatalf("setup remove: %v", err)
		}
		fired := record(svc)
		snaps := svc.ListSourceSnapshots(doc)
		if len(snaps) == 0 {
			t.Fatal("no snapshot to restore")
		}
		if _, err := svc.RestoreSourceSnapshot(context.Background(), doc, snaps[0].ID); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if *fired != 1 {
			t.Errorf("invalidations = %d, want 1", *fired)
		}
	})
}

// TestEditorMutations_ChatContextGenerationBumped proves the consumer side:
// with the flow caches subscribed as DI subscribes them, an editor mutation
// bumps the per-flow generation that keys the scrubbed-context cache — the
// next AI turn rebuilds against the post-edit flow.
func TestEditorMutations_ChatContextGenerationBumped(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	chat := &ChatService{chatCtxCache: newChatContextCache()}
	svc.bus = subscribedBus(svc, NewDashboardService(nil, nil, svc, nil), chat)

	genBefore := chat.chatCtxGen.Load(doc.ID)
	if _, err := svc.RemoveBlock(context.Background(), doc, blockIDByURL(doc, "https://target")); err != nil {
		t.Fatalf("RemoveBlock: %v", err)
	}
	if got := chat.chatCtxGen.Load(doc.ID); got != genBefore+1 {
		t.Errorf("chat generation = %d, want %d", got, genBefore+1)
	}
}
