package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/testutil"
	"pad-core/parser"
)

// TestEditorMutations_InvalidateDerivedCaches is the R3.3 coherence gate:
// EVERY editor mutation path must drop derived caches (search index + the
// registered consumers — the chat context's scrubbed-doc cache key rides the
// same callback). Before this fix, PatchFlow/persistCloudDoc never
// invalidated: a user could delete a block, then ask the AI about the flow
// and get answers describing the deleted block (cached pre-edit context).
func TestEditorMutations_InvalidateDerivedCaches(t *testing.T) {
	record := func(svc *FlowService) *int {
		fired := 0
		svc.OnInvalidateFlow(func(flowID string) { fired++ })
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
		if _, err := svc.ApplyFix(context.Background(), doc, target, "wrap-error-handler", "unhandled-error", "", ""); err != nil {
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
// with a ChatService registered through OnInvalidateFlow (the production DI
// wiring), an editor mutation bumps the per-flow generation counter that keys
// the scrubbed-context cache — the next AI turn rebuilds against the
// post-edit flow.
func TestEditorMutations_ChatContextGenerationBumped(t *testing.T) {
	svc, doc := newDesktopEditSvc(t)
	chat := &ChatService{chatCtxCache: newChatContextCache(), chatCtxGen: newChatCtxGenCache()}
	svc.OnInvalidateFlow(chat.InvalidateChatContext)

	genBefore := uint64(0)
	if v, ok := chat.chatCtxGen.Get(context.Background(), doc.ID); ok {
		genBefore = v.(uint64)
	}
	if _, err := svc.RemoveBlock(context.Background(), doc, blockIDByURL(doc, "https://target")); err != nil {
		t.Fatalf("RemoveBlock: %v", err)
	}
	v, ok := chat.chatCtxGen.Get(context.Background(), doc.ID)
	if !ok || v.(uint64) != genBefore+1 {
		t.Errorf("chat generation = %v (ok=%v), want %d", v, ok, genBefore+1)
	}
}
