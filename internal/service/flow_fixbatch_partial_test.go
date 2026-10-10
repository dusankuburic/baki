package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"

	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"
)

// redundantSetsSrc is a flow of n redundant self-assignments: n fixable
// findings (redundant-action removes the line), one fix per loop iteration.
func redundantSetsSrc(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#Region %q\n", name)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "SET %s%d TO %%%s%d%%\n", name, i, name, i)
	}
	b.WriteString("#EndRegion\n")
	return b.String()
}

// passCancelRule cancels the context during the onPass-th analysis pass over
// a document named flowName ("" = any). A pass is one analysis of one parsed
// document; the fix loop parses afresh every iteration, so distinct
// *FlowDocument values count the passes — a cancel on pass 3 lands after
// exactly two fixes, whatever those fixes did to the source. The wrapped rule
// keeps its ID, so the loop still dispatches it.
type passCancelRule struct {
	analyzer.BlockRule
	cancel   func()
	onPass   int
	flowName string
	mu       sync.Mutex
	seen     map[*models.FlowDocument]bool
}

func (r *passCancelRule) Check(b *models.Block, ctx *analyzer.RuleContext) []models.Finding {
	if r.flowName == "" || (ctx.Flow != nil && ctx.Flow.Name == r.flowName) {
		r.mu.Lock()
		if !r.seen[ctx.Flow] {
			r.seen[ctx.Flow] = true
			if len(r.seen) == r.onPass {
				r.cancel()
			}
		}
		r.mu.Unlock()
	}
	return r.BlockRule.Check(b, ctx)
}

// cancellingResolver is a rule-profile resolver pre-resolved for org "" (the
// desktop/local org) whose rule set cancels ctx on the given analysis pass
// over flowName. It is how a test reaches the fix loop's rule dispatch
// through the service (fixOptions takes the profile's rules verbatim).
func cancellingResolver(cancel func(), pass int, flowName string) *RuleProfileResolver {
	rules := analyzer.AllRules()
	for i, r := range rules {
		if r.ID() == "redundant-action" {
			rules[i] = &passCancelRule{BlockRule: r.(analyzer.BlockRule), cancel: cancel, onPass: pass, flowName: flowName, seen: map[*models.FlowDocument]bool{}}
		}
	}
	r := NewRuleProfileResolver(nil, nil, nil)
	k, _ := deploymentProfile.Key()
	r.store.m[k] = &cachedProfile{profile: &RuleProfile{Settings: nil, Rules: rules}, resolved: time.Now()}
	return r
}

func countSets(s string) int { return strings.Count(s, "\nSET ") }

// TestApplyFixBatchResult_DesktopPersistsFixesAppliedBeforeDeadline: the request context
// ends the loop after two of six fixes. The batch used to return the context error and
// persist NOTHING — a 100-fix "fix all" on a multi-MB flow needs minutes against the
// route's 30 s deadline, so every attempt burned the deadline for zero change. The two
// fixes must be written, the result marked partial with the reason, the undo entry
// recorded, and the returned document must be the re-parse of what was written.
func TestApplyFixBatchResult_DesktopPersistsFixesAppliedBeforeDeadline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Main.txt")
	orig := redundantSetsSrc("Main", 6)
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, nil)
	doc, err := svc.LoadFlowFromPath(path)
	if err != nil {
		t.Fatalf("LoadFlowFromPath: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.SetRuleProfileResolver(cancellingResolver(cancel, 3, ""))

	out, err := svc.ApplyFixBatchResult(ctx, doc, nil, 50)
	if err != nil {
		t.Fatalf("ApplyFixBatchResult returned %v: the fixes applied before the deadline were discarded", err)
	}
	if !out.Partial || out.Applied != 2 || !errors.Is(out.Reason, context.Canceled) {
		t.Fatalf("result = partial %v, applied %d, reason %v; want partial, 2, context.Canceled", out.Partial, out.Applied, out.Reason)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if n := countSets(string(data)); n != 4 {
		t.Fatalf("Main.txt has %d SETs, want 4 (two fixes persisted, the rest untouched):\n%s", n, data)
	}
	if out.Doc == nil || out.Doc.Metadata.BlockCount != doc.Metadata.BlockCount-2 || out.Doc.FilePath != path {
		t.Fatalf("returned doc = %+v, want the re-parse of the written file (%d blocks)", out.Doc, doc.Metadata.BlockCount-2)
	}
	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) != 1 || snaps[0].source != orig || snaps[0].Label != "before batch fix" {
		t.Fatalf("want one 'before batch fix' snapshot of the original bytes, got %+v", snaps)
	}

	// The narrow wrapper reports the same partial batch as a success.
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	svc.SetRuleProfileResolver(cancellingResolver(cancel2, 3, ""))
	if _, applied, err := svc.ApplyFixBatch(ctx2, doc, nil, 50); err != nil || applied != 2 {
		t.Fatalf("ApplyFixBatch = %d, %v; want 2 applied and no error", applied, err)
	}

	// A deadline before the FIRST fix is still an error and changes nothing.
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	svc.SetRuleProfileResolver(cancellingResolver(func() {}, 0, ""))
	out, err = svc.ApplyFixBatchResult(dead, doc, nil, 50)
	if !errors.Is(err, context.Canceled) || out.Partial || out.Applied != 0 {
		t.Fatalf("pre-expired context: err=%v partial=%v applied=%d; want Canceled, not partial, 0", err, out.Partial, out.Applied)
	}
	if data, _ := os.ReadFile(path); string(data) != orig { // #nosec G304 -- test temp dir
		t.Fatal("a batch with nothing applied changed the file")
	}
}

// deadCtxRejectingBackend fails any call made with a done context, like a
// real database driver does. The request context that cut a batch short is
// done, so persisting the partial batch under it would fail — the persist
// must run under a detached context.
type deadCtxRejectingBackend struct{ *testutil.FakeBackend }

func (b deadCtxRejectingBackend) LoadFlowHeader(ctx context.Context, id string) (*storageif.FlowDocument, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.FakeBackend.LoadFlowHeader(ctx, id)
}

func (b deadCtxRejectingBackend) SaveFlow(ctx context.Context, f *storageif.FlowDocument) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.FakeBackend.SaveFlow(ctx, f)
}

// TestApplyFixBatchResult_CloudPersistsPartialUnderDetachedContext: the cloud
// path persists via the storage backend, which rejects a done context. The
// two fixes applied before the cancel must still land in the stored source.
func TestApplyFixBatchResult_CloudPersistsPartialUnderDetachedContext(t *testing.T) {
	orig := redundantSetsSrc("Main", 6)
	doc, err := parser.ParseText(orig, "Main.txt", int64(len(orig)))
	if err != nil {
		t.Fatal(err)
	}
	doc.ID, doc.OwnerID, doc.FilePath, doc.Source = "cloud-flow", "u1", "", orig
	doc.RebuildIndexes()
	fake := testutil.NewFakeBackend()
	fake.Flows["cloud-flow"] = &storageif.FlowDocument{ID: "cloud-flow", Name: "Main", OwnerID: "u1", Content: []byte(`{}`), Source: orig}
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, ldp, deadCtxRejectingBackend{fake}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.SetRuleProfileResolver(cancellingResolver(cancel, 3, ""))

	out, err := svc.ApplyFixBatchResult(ctx, doc, nil, 50)
	if err != nil {
		t.Fatalf("ApplyFixBatchResult: %v (the partial batch was persisted under the dead request context)", err)
	}
	if !out.Partial || out.Applied != 2 {
		t.Fatalf("result = partial %v, applied %d; want partial with 2", out.Partial, out.Applied)
	}
	stored := fake.Flows["cloud-flow"]
	if stored == nil || countSets(stored.Source) != 4 {
		t.Fatalf("stored source has %d SETs, want 4", countSets(stored.Source))
	}
	if stored.Version != 1 {
		t.Fatalf("stored version = %d, want 1 (one save)", stored.Version)
	}
	if len(svc.ListSourceSnapshots(doc)) != 1 {
		t.Fatalf("want one undo entry for the partial batch, got %d", len(svc.ListSourceSnapshots(doc)))
	}
}

// TestApplyFixBatchResult_DesktopFolderKeepsMembersFixedBeforeDeadline: the
// per-member loop hits the deadline in the first member after two of its
// fixes. Those two are kept, the member after it is not attempted (left
// byte-identical), the undo entry covers only the rewritten member, and the
// result is partial.
func TestApplyFixBatchResult_DesktopFolderKeepsMembersFixedBeforeDeadline(t *testing.T) {
	dir := t.TempDir()
	mainSrc, subSrc := redundantSetsSrc("Main", 4), redundantSetsSrc("Sub", 2)
	writeMembers(t, dir, map[string]string{"Main.txt": mainSrc, "Sub.txt": subSrc})
	svc := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, nil)
	doc, err := svc.LoadFlowFolder(dir)
	if err != nil {
		t.Fatalf("LoadFlowFolder: %v", err)
	}
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
	if got := readMember(t, dir, "Main.txt"); countSets(got) != 2 {
		t.Fatalf("Main.txt has %d SETs, want 2:\n%s", countSets(got), got)
	}
	if got := readMember(t, dir, "Sub.txt"); got != subSrc {
		t.Fatalf("Sub.txt was changed although the deadline hit before it:\n%s", got)
	}
	if out.Doc == nil || !out.Doc.IsFolder || out.Doc.Metadata.BlockCount != doc.Metadata.BlockCount-2 {
		t.Fatalf("returned doc = %+v, want the re-merged folder with 2 blocks fewer", out.Doc)
	}
	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) != 1 || snaps[0].Files["Main.txt"] != mainSrc || len(snaps[0].Files) != 1 {
		t.Fatalf("want one folder snapshot carrying Main.txt only, got %+v", snaps)
	}

	// A deadline that hits in a LATER member keeps the earlier member's
	// complete fixes: Sub's first pass is cancelled, Main's two fixes stay.
	writeMembers(t, dir, map[string]string{"Main.txt": redundantSetsSrc("Main", 2), "Sub.txt": subSrc})
	doc, err = svc.LoadFlowFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	svc.SetRuleProfileResolver(cancellingResolver(cancel2, 1, "Sub"))
	out, err = svc.ApplyFixBatchResult(ctx2, doc, nil, 50)
	if err != nil || !out.Partial || out.Applied != 2 {
		t.Fatalf("later-member deadline: result = %+v, %v; want partial with Main's 2 fixes", out, err)
	}
	if got := readMember(t, dir, "Main.txt"); countSets(got) != 0 {
		t.Fatalf("Main.txt has %d SETs, want 0", countSets(got))
	}
	if got := readMember(t, dir, "Sub.txt"); got != subSrc {
		t.Fatalf("Sub.txt was changed by a cancelled first pass:\n%s", got)
	}

	// A deadline before ANY member got a fix is still an error, nothing written.
	writeMembers(t, dir, map[string]string{"Main.txt": mainSrc, "Sub.txt": subSrc})
	doc, err = svc.LoadFlowFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	svc.SetRuleProfileResolver(cancellingResolver(func() {}, 0, ""))
	out, err = svc.ApplyFixBatchResult(dead, doc, nil, 50)
	if !errors.Is(err, context.Canceled) || out.Partial || out.Applied != 0 {
		t.Fatalf("pre-expired context: err=%v partial=%v applied=%d; want Canceled, not partial, 0", err, out.Partial, out.Applied)
	}
	if readMember(t, dir, "Main.txt") != mainSrc || readMember(t, dir, "Sub.txt") != subSrc {
		t.Fatal("a folder batch with nothing applied changed a member")
	}
}
