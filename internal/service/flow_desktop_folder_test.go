package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/testutil"

	"pad-core/analyzer"
	"pad-core/models"
)

// A two-file desktop folder where the ONLY auto-fixable finding lives in the
// subflow file: Main.txt is clean, Subflow.txt carries a redundant
// self-assignment (redundant-action, auto-fix removes the line).
const (
	desktopMainSrc    = "#Region \"Main\"\nWAIT 1\n#EndRegion\n"
	desktopSubflowSrc = "#Region \"Subflow\"\nSET X TO %X%\n#EndRegion\n"
)

func newDesktopFolder(t *testing.T) (*FlowService, *models.FlowDocument, string) {
	t.Helper()
	dir := t.TempDir()
	for name, src := range map[string]string{"Main.txt": desktopMainSrc, "Subflow.txt": desktopSubflowSrc} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	svc := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, nil)
	doc, err := svc.LoadFlowFolder(dir)
	if err != nil {
		t.Fatalf("LoadFlowFolder: %v", err)
	}
	return svc, doc, dir
}

func readMember(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// TestApplyFixBatch_DesktopFolder_PatchesSubflowFile pins the local twin of the
// cloud per-file loop. The desktop branch used to read and write Main.txt
// only, so a batch on a folder flow reported "0 applied" for findings that
// lived in any other member file — silently, with the UI showing the findings
// still open.
func TestApplyFixBatch_DesktopFolder_PatchesSubflowFile(t *testing.T) {
	svc, doc, dir := newDesktopFolder(t)

	updated, applied, err := svc.ApplyFixBatch(context.Background(), doc, map[string]bool{"redundant-action": true}, 10)
	if err != nil {
		t.Fatalf("ApplyFixBatch: %v", err)
	}
	if applied == 0 {
		t.Fatal("expected the subflow-file finding to be fixed, got 0 applied")
	}
	if got := readMember(t, dir, "Subflow.txt"); strings.Contains(got, "SET X TO %X%") {
		t.Errorf("Subflow.txt still contains the redundant SET:\n%s", got)
	}
	if got := readMember(t, dir, "Main.txt"); got != desktopMainSrc {
		t.Errorf("Main.txt was rewritten although it had nothing to fix:\n%s", got)
	}
	if updated == nil || !updated.IsFolder || len(updated.Files) != 2 {
		t.Fatalf("expected the re-parsed folder doc (2 files), got %+v", updated)
	}

	// One undo entry covering the touched member; restoring it puts the
	// original bytes back and re-parses the folder.
	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) != 1 {
		t.Fatalf("expected exactly one folder snapshot, got %d", len(snaps))
	}
	if _, ok := snaps[0].Files["Subflow.txt"]; !ok || snaps[0].TargetPath != dir {
		t.Fatalf("snapshot should record Subflow.txt under the folder path, got %+v", snaps[0])
	}
	if _, ok := snaps[0].Files["Main.txt"]; ok {
		t.Error("snapshot should only carry the files the batch touched")
	}
	restored, err := svc.RestoreSourceSnapshot(context.Background(), updated, snaps[0].ID)
	if err != nil {
		t.Fatalf("RestoreSourceSnapshot: %v", err)
	}
	if got := readMember(t, dir, "Subflow.txt"); got != desktopSubflowSrc {
		t.Errorf("restore did not put Subflow.txt back:\n%s", got)
	}
	if restored == nil || !restored.IsFolder {
		t.Errorf("restore should return the re-parsed folder doc, got %+v", restored)
	}
}

// TestGetSaveSource_DesktopFolderMember covers the per-file source editor
// contract: the member named by `file` is read and written, Main.txt stays
// the default, and names that are not real members of the folder are
// rejected before any I/O (traversal, unknown file).
func TestGetSaveSource_DesktopFolderMember(t *testing.T) {
	svc, doc, dir := newDesktopFolder(t)
	ctx := context.Background()

	if got, err := svc.GetSource(doc, ""); err != nil || got != desktopMainSrc {
		t.Fatalf("GetSource(\"\") = %q, %v; want Main.txt", got, err)
	}
	if got, err := svc.GetSource(doc, "Subflow.txt"); err != nil || got != desktopSubflowSrc {
		t.Fatalf("GetSource(Subflow.txt) = %q, %v", got, err)
	}
	for _, bad := range []string{"../Subflow.txt", "sub/../Main.txt", "Nope.txt", "Main.pad", "..", "notes.md"} {
		if _, err := svc.GetSource(doc, bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("GetSource(%q) error = %v, want ErrInvalidInput", bad, err)
		}
		if _, err := svc.SaveSource(ctx, doc, "#Region \"X\"\n#EndRegion\n", bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("SaveSource(%q) error = %v, want ErrInvalidInput", bad, err)
		}
	}

	const edited = "#Region \"Subflow\"\nWAIT 2\n#EndRegion\n"
	updated, err := svc.SaveSource(ctx, doc, edited, "Subflow.txt")
	if err != nil {
		t.Fatalf("SaveSource(Subflow.txt): %v", err)
	}
	if got := readMember(t, dir, "Subflow.txt"); got != edited {
		t.Errorf("Subflow.txt = %q, want the edited source", got)
	}
	if got := readMember(t, dir, "Main.txt"); got != desktopMainSrc {
		t.Errorf("saving Subflow.txt overwrote Main.txt:\n%s", got)
	}
	if updated == nil || !updated.IsFolder {
		t.Errorf("SaveSource should return the re-parsed folder, got %+v", updated)
	}
}

// TestApplyFix_BehaviourChangingRequiresUnsafe is the service half of the F6
// gate: a behaviour-changing fixer (replace-with-variable, the auto-fix for
// hardcoded-url) is refused without the explicit opt-in and applied with it.
func TestApplyFix_BehaviourChangingRequiresUnsafe(t *testing.T) {
	svc, doc := newFixBatchSvc(t, newTestSettingsStore(t))
	report := analyzer.RunAnalysis(doc, analyzer.AllRules(), models.DefaultSettings(), nil)
	var target *models.Finding
	for i := range report.Findings {
		if report.Findings[i].AutoFix == "replace-with-variable" {
			target = &report.Findings[i]
			break
		}
	}
	if target == nil {
		t.Fatalf("fixture has rotted: no replace-with-variable finding in %+v", report.Findings)
	}
	prop, _ := target.Metadata["property"].(string)

	_, err := svc.ApplyFix(context.Background(), doc, target.BlockID, target.AutoFix, target.RuleID, "", prop, false)
	if !errors.Is(err, ErrUnsafeFixRequiresOptIn) || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ApplyFix without unsafe: err = %v, want ErrUnsafeFixRequiresOptIn (an ErrInvalidInput)", err)
	}
	if !strings.Contains(err.Error(), "replace-with-variable") {
		t.Errorf("error should name the fix type: %v", err)
	}

	if _, err := svc.ApplyFix(context.Background(), doc, target.BlockID, target.AutoFix, target.RuleID, "", prop, true); err != nil {
		t.Fatalf("ApplyFix with unsafe=true: %v", err)
	}
}
