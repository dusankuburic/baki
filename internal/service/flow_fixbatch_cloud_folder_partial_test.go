package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
	"pad-core/parser"
)

// storedFolderMembers reads the members of the folder flow the fake backend
// holds under id (its content is the merged document's JSON).
func storedFolderMembers(t *testing.T, fake *testutil.FakeBackend, id string) map[string]string {
	t.Helper()
	row := fake.Flows[id]
	if row == nil {
		t.Fatalf("flow %s not stored", id)
	}
	var doc models.FlowDocument
	if err := json.Unmarshal(row.Content, &doc); err != nil {
		t.Fatalf("stored content: %v", err)
	}
	return parser.SerializeFiles(&doc)
}

// TestApplyFixBatchResult_CloudFolderKeepsMembersFixedBeforeDeadline: the cloud twin of
// the desktop folder test. The per-member loop hits the deadline in the first member
// after two of its fixes: those two are kept, the next member is left as it was, the
// merged folder is persisted under a detached context (the backend rejects the dead
// request context), the undo entry is recorded, and the result is partial; a deadline
// before any fix is still an error with nothing stored.
func TestApplyFixBatchResult_CloudFolderKeepsMembersFixedBeforeDeadline(t *testing.T) {
	mainSrc, subSrc := redundantSetsSrc("Main", 4), redundantSetsSrc("Sub", 2)
	doc, err := parser.ParseFiles(map[string]string{"Main.txt": mainSrc, "Sub.txt": subSrc}, "Folder")
	if err != nil {
		t.Fatal(err)
	}
	if !doc.IsFolder {
		t.Fatal("ParseFiles of two members did not produce a folder flow")
	}
	doc.ID, doc.OwnerID, doc.FilePath = "cloud-folder", "u1", ""
	doc.RebuildIndexes()
	fake := testutil.NewFakeBackend()
	fake.Flows["cloud-folder"] = &storageif.FlowDocument{ID: "cloud-folder", Name: "Folder", OwnerID: "u1", Content: []byte(`{}`)}
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, ldp, deadCtxRejectingBackend{fake}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Members run in name order: Main.txt, then Sub.txt. Cancel on Main's
	// third pass, i.e. after its second fix.
	svc.SetRuleProfileResolver(cancellingResolver(cancel, 3, "Main"))

	out, err := svc.ApplyFixBatchResult(ctx, doc, nil, 50)
	if err != nil {
		t.Fatalf("ApplyFixBatchResult returned %v: the member fixed before the deadline was discarded", err)
	}
	if !out.Partial || out.Applied != 2 || !errors.Is(out.Reason, context.Canceled) {
		t.Fatalf("result = partial %v, applied %d, reason %v; want partial, 2, context.Canceled", out.Partial, out.Applied, out.Reason)
	}
	members := storedFolderMembers(t, fake, "cloud-folder")
	// The stored members are re-serialised (indented), so count statements,
	// not line starts.
	if got := members["Main.txt"]; strings.Count(got, "SET ") != 2 {
		t.Fatalf("stored Main.txt has %d SETs, want 2:\n%s", strings.Count(got, "SET "), got)
	}
	if got := members["Sub.txt"]; strings.Count(got, "SET ") != strings.Count(subSrc, "SET ") {
		t.Fatalf("stored Sub.txt was changed although the deadline hit before it:\n%s", got)
	}
	if v := fake.Flows["cloud-folder"].Version; v != 1 {
		t.Fatalf("stored version = %d, want 1 (one save)", v)
	}
	if out.Doc == nil || !out.Doc.IsFolder || out.Doc.Metadata.BlockCount != doc.Metadata.BlockCount-2 {
		t.Fatalf("returned doc = %+v, want the re-merged folder with 2 blocks fewer", out.Doc)
	}
	if n := len(snapshotsOf(t, svc, doc)); n != 1 {
		t.Fatalf("want one undo entry for the partial batch, got %d", n)
	}

	// A deadline before the first fix: nothing applied, nothing stored.
	fresh, err := parser.ParseFiles(map[string]string{"Main.txt": mainSrc, "Sub.txt": subSrc}, "Folder")
	if err != nil {
		t.Fatal(err)
	}
	fresh.ID, fresh.OwnerID, fresh.FilePath = "cloud-folder-2", "u1", ""
	fresh.RebuildIndexes()
	fake.Flows["cloud-folder-2"] = &storageif.FlowDocument{ID: "cloud-folder-2", Name: "Folder", OwnerID: "u1", Content: []byte(`{}`)}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	svc.SetRuleProfileResolver(cancellingResolver(cancel2, 1, "Main"))
	if _, err := svc.ApplyFixBatchResult(ctx2, fresh, nil, 50); !errors.Is(err, context.Canceled) {
		t.Fatalf("deadline before any fix: err = %v, want context.Canceled", err)
	}
	if v := fake.Flows["cloud-folder-2"].Version; v != 0 {
		t.Fatalf("a batch with nothing applied saved the flow (version %d)", v)
	}
}
