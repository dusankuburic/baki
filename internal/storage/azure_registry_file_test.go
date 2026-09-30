package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/interfaces"
)

var personal = interfaces.AzureUserOwner("")

func newFileRegistry(t *testing.T, dir string) *FileAzureRegistry {
	t.Helper()
	s, err := NewFileAzureRegistry(dir)
	if err != nil {
		t.Fatalf("NewFileAzureRegistry: %v", err)
	}
	return s
}

// fileRevision reads the revision counter on disk.
func fileRevision(t *testing.T, dir string) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, AzureRegistryFileName))
	if err != nil {
		t.Fatalf("read registry file: %v", err)
	}
	var head struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatalf("parse registry file: %v", err)
	}
	return head.Revision
}

// tempLeftovers lists write temps left in dir (a failed write must remove
// its own).
func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".azure-models-tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// TestFileAzureRegistry_Contract runs the shared registry contract on the
// file store: its only owner is the personal "" (no organisations on the
// desktop), and every scenario starts from a fresh directory.
func TestFileAzureRegistry_Contract(t *testing.T) {
	contract.RunAzureRegistrySuite(t, contract.AzureRegistryHarness{
		Fresh: func(t *testing.T, kind interfaces.AzureOwnerKind) (interfaces.AzureRegistryStore, interfaces.AzureOwner, bool) {
			if kind != interfaces.AzureOwnerUser {
				return nil, interfaces.AzureOwner{}, false
			}
			return newFileRegistry(t, t.TempDir()), personal, true
		},
	})
}

// TestFileAzureRegistry_OnlyThePersonalOwner: the file has no owner column,
// so any owner but the personal "" must be refused rather than silently
// sharing one registry between them.
func TestFileAzureRegistry_OnlyThePersonalOwner(t *testing.T) {
	ctx := context.Background()
	s := newFileRegistry(t, t.TempDir())
	for _, owner := range []interfaces.AzureOwner{interfaces.AzureUserOwner("u1"), interfaces.AzureOrgOwner("org-1"), {Kind: "team", ID: ""}} {
		if _, err := s.ListConnections(ctx, owner); !errors.Is(err, interfaces.ErrAzureRegistryOwnerUnsupported) {
			t.Errorf("ListConnections(%s) = %v, want ErrAzureRegistryOwnerUnsupported", owner, err)
		}
		if _, err := s.CreateConnection(ctx, owner, interfaces.AzureConnectionRecord{ID: "default", Name: "x"}); !errors.Is(err, interfaces.ErrAzureRegistryOwnerUnsupported) {
			t.Errorf("CreateConnection(%s) = %v, want ErrAzureRegistryOwnerUnsupported", owner, err)
		}
		if err := s.WithOwnerLock(ctx, owner, func(context.Context) error { return nil }); !errors.Is(err, interfaces.ErrAzureRegistryOwnerUnsupported) {
			t.Errorf("WithOwnerLock(%s) = %v, want ErrAzureRegistryOwnerUnsupported", owner, err)
		}
	}
}

// TestFileAzureRegistry_ProcessMutexLosesNoWrite: concurrent writers in one
// process each re-read the file inside the mutex, so none overwrites
// another's row (a read-modify-write outside it would keep only the last).
func TestFileAzureRegistry_ProcessMutexLosesNoWrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := newFileRegistry(t, dir)
	if _, err := s.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "default", Name: "res"}); err != nil {
		t.Fatal(err)
	}
	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers*2)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := s.CreateModel(ctx, personal, interfaces.AzureModelRecord{
				ID: contract.NewAzureTestID(), ConnectionID: "default", Kind: interfaces.AzureKindChat,
				Deployment: fmt.Sprintf("dep-%02d", i), DisplayName: "d", Purpose: interfaces.AzurePurposeGeneral,
			})
			if err != nil {
				errs <- err
				return
			}
			errs <- s.RecordModelTest(ctx, personal, m.ID, &interfaces.AzureTestResult{OK: true, LatencyMs: i})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	}
	models, err := s.ListModels(ctx, personal)
	if err != nil || len(models) != writers {
		t.Fatalf("models = %d (%v), want %d: a concurrent write was lost", len(models), err, writers)
	}
	for _, m := range models {
		if m.LastTest == nil {
			t.Errorf("model %s lost its test stamp", m.Deployment)
		}
	}
	// One create and one stamp per writer, plus the resource.
	if rev := fileRevision(t, dir); rev != 2*writers+1 {
		t.Errorf("revision = %d, want %d (one per committed write)", rev, 2*writers+1)
	}
}

// TestFileAzureRegistry_SecondWriterConflicts: a second process (here a
// second store instance, with its own mutex) that writes between this
// write's read and its rename must turn this write into
// ErrAzureRegistryConflict — never silently discard the other write.
func TestFileAzureRegistry_SecondWriterConflicts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, b := newFileRegistry(t, dir), newFileRegistry(t, dir)
	if _, err := a.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "default", Name: "first"}); err != nil {
		t.Fatal(err)
	}
	fired := false
	a.beforeCommit = func() {
		if fired {
			return
		}
		fired = true
		if _, err := b.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "bbbbbbbbbb", Name: "from-b"}); err != nil {
			t.Errorf("the other writer: %v", err)
		}
	}
	_, err := a.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "aaaaaaaaaa", Name: "from-a"})
	if !errors.Is(err, interfaces.ErrAzureRegistryConflict) {
		t.Fatalf("a write racing another process = %v, want ErrAzureRegistryConflict", err)
	}
	if left := tempLeftovers(t, dir); len(left) != 0 {
		t.Errorf("the refused write left its temp file: %v", left)
	}
	list, _ := b.ListConnections(ctx, personal)
	if len(list) != 2 || list[1].ID != "bbbbbbbbbb" {
		t.Errorf("after the conflict: %+v, want the other writer's row kept and ours absent", list)
	}
	// The retry re-reads and lands on top of the other write.
	if _, err := a.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "aaaaaaaaaa", Name: "from-a"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if list, _ := b.ListConnections(ctx, personal); len(list) != 3 {
		t.Errorf("after the retry: %d resources, want 3", len(list))
	}
}

// TestFileAzureRegistry_RenameRetriesOverAnOpenReader: on Windows a reader
// holding the file open (Go's os.Open shares no delete access) makes the
// replacing rename fail transiently; the store's write rides the bounded
// retry instead of failing. Elsewhere the rename simply succeeds.
func TestFileAzureRegistry_RenameRetriesOverAnOpenReader(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := newFileRegistry(t, dir)
	if _, err := s.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "default", Name: "res"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, AzureRegistryFileName))
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = f.Close()
		close(released)
	}()
	if _, err := s.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "abcdefghij", Name: "second"}); err != nil {
		t.Fatalf("write over a file a reader holds open: %v", err)
	}
	<-released
	if list, _ := s.ListConnections(ctx, personal); len(list) != 2 {
		t.Errorf("resources = %d, want 2", len(list))
	}
	if left := tempLeftovers(t, dir); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

// TestFileAzureRegistry_CorruptFileIsQuarantined: an unreadable file is
// moved aside — the copy kept byte for byte — and the registry starts
// empty with registryState recovered (sticky, so the UI can say where the
// copy is). A structurally valid file that breaks a registry rule is
// corrupt too: serving it would expose state no write could produce.
func TestFileAzureRegistry_CorruptFileIsQuarantined(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, body string }{
		{"not JSON", `{"version": 1, "revision": 3, "connections": [`},
		{"no version", `{"revision": 3, "connections": [], "models": []}`},
		{"wrong shape", `{"version": 1, "revision": 3, "connections": {"id": "default"}}`},
		{"an orphan model", `{"version": 1, "revision": 3, "connections": [], "models": [
			{"id": "abcdefghij", "connectionId": "default", "kind": "chat", "deployment": "gpt-4.1",
			 "displayName": "GPT", "purpose": "general", "createdAt": "2026-09-30T10:00:00Z", "updatedAt": "2026-09-30T10:00:00Z"}]}`},
		{"an invalid resource id", `{"version": 1, "revision": 3, "connections": [
			{"id": "NOT-VALID", "name": "x", "createdAt": "2026-09-30T10:00:00Z", "updatedAt": "2026-09-30T10:00:00Z"}], "models": []}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, AzureRegistryFileName)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			s := newFileRegistry(t, dir)
			st := s.State(ctx)
			if st.State != interfaces.AzureRegistryRecovered || st.QuarantinePath == "" {
				t.Fatalf("State = %+v, want recovered with the copy's path", st)
			}
			kept, err := os.ReadFile(st.QuarantinePath)
			if err != nil || !bytes.Equal(kept, []byte(tc.body)) {
				t.Errorf("quarantined copy = (%q, %v), want the original bytes", kept, err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the corrupt file is still in place: %v", err)
			}
			if list, err := s.ListConnections(ctx, personal); err != nil || len(list) != 0 {
				t.Errorf("after recovery: (%v, %v), want an empty registry", list, err)
			}
			if _, err := s.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "default", Name: "again"}); err != nil {
				t.Fatalf("a write after recovery: %v", err)
			}
			if st2 := s.State(ctx); st2 != st {
				t.Errorf("State after a new write = %+v, want the recovered state kept (%+v)", st2, st)
			}
			// Another instance on the now-healthy file reports ok.
			if st3 := newFileRegistry(t, dir).State(ctx); st3.State != interfaces.AzureRegistryOK {
				t.Errorf("a fresh instance on the rewritten file = %+v, want ok", st3)
			}
		})
	}
}

// TestFileAzureRegistry_NewerVersionIsReadOnly: a file a newer app wrote is
// shown (what this version understands of it) but never rewritten, and
// never quarantined — that would hide its data from the newer app.
func TestFileAzureRegistry_NewerVersionIsReadOnly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		body      string
		wantConns int
	}{
		{"readable", `{"version": 2, "revision": 9, "futureField": {"x": 1}, "connections": [
			{"id": "default", "name": "from-the-future", "createdAt": "2026-09-30T10:00:00Z", "updatedAt": "2026-09-30T10:00:00Z", "newKnob": true}],
			"models": []}`, 1},
		{"unreadable shape", `{"version": 7, "revision": 1, "connections": "moved to v7"}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, AzureRegistryFileName)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			s := newFileRegistry(t, dir)
			if st := s.State(ctx); st.State != interfaces.AzureRegistryReadOnly {
				t.Fatalf("State = %+v, want read_only", st)
			}
			list, err := s.ListConnections(ctx, personal)
			if err != nil || len(list) != tc.wantConns {
				t.Errorf("ListConnections = (%+v, %v), want %d", list, err, tc.wantConns)
			}
			_, err = s.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "abcdefghij", Name: "new"})
			if !errors.Is(err, interfaces.ErrAzureRegistryReadOnly) {
				t.Errorf("a write = %v, want ErrAzureRegistryReadOnly", err)
			}
			if _, err := s.MarkImported(ctx, personal); !errors.Is(err, interfaces.ErrAzureRegistryReadOnly) {
				t.Errorf("MarkImported = %v, want ErrAzureRegistryReadOnly", err)
			}
			if data, _ := os.ReadFile(path); !bytes.Equal(data, []byte(tc.body)) {
				t.Error("a read-only file was rewritten")
			}
			if matches, _ := filepath.Glob(path + ".corrupt-*"); len(matches) != 0 {
				t.Errorf("a newer file was quarantined: %v", matches)
			}
		})
	}
}

// TestFileAzureRegistry_PendingRowsSurviveWithTheirBootID: the desktop's
// two-step create persists the pending row with the process's boot id, so
// the next process can tell a create that died mid-way (another boot id)
// from one still in flight in this process.
func TestFileAzureRegistry_PendingRowsSurviveWithTheirBootID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := newFileRegistry(t, dir)
	if !first.PendingWrites() || first.BootID() == "" {
		t.Fatalf("PendingWrites %v, BootID %q: the file store must use the pending protocol", first.PendingWrites(), first.BootID())
	}
	if _, err := first.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "abcdefghij", Name: "died", Pending: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "klmnopqrst", Name: "finished", Pending: true}); err != nil {
		t.Fatal(err)
	}
	if err := first.FinalizeConnection(ctx, personal, "klmnopqrst"); err != nil {
		t.Fatal(err)
	}

	second := newFileRegistry(t, dir) // the next process
	if second.BootID() == first.BootID() {
		t.Fatal("two store instances share a boot id")
	}
	list, err := second.ListConnections(ctx, personal)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListConnections = (%+v, %v)", list, err)
	}
	byID := map[string]interfaces.AzureConnectionRecord{list[0].ID: list[0], list[1].ID: list[1]}
	if died := byID["abcdefghij"]; !died.Pending || died.BootID != first.BootID() {
		t.Errorf("the unfinished row = %+v, want pending with the first boot id %q", died, first.BootID())
	}
	if done := byID["klmnopqrst"]; done.Pending || done.BootID != "" {
		t.Errorf("the finalised row = %+v, want no pending state", done)
	}
	// The service rolls the stale row back by deleting it.
	if _, err := second.DeleteConnection(ctx, personal, "abcdefghij"); err != nil {
		t.Fatalf("roll back the stale pending row: %v", err)
	}
	if n, _ := second.Counts(ctx, personal); n.Connections != 1 {
		t.Errorf("resources after the rollback = %d, want 1", n.Connections)
	}
}

// TestFileAzureRegistry_RoundTripsEveryField: what one process writes, the
// next reads back field for field.
func TestFileAzureRegistry_RoundTripsEveryField(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a := newFileRegistry(t, dir)
	if _, err := a.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "default", Name: "my-res"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetDiscoveryDismissed(ctx, personal, "default", true); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordConnectionTest(ctx, personal, "default", &interfaces.AzureTestResult{OK: false, LatencyMs: 12, Error: "403: missing role"}); err != nil {
		t.Fatal(err)
	}
	chat, err := a.CreateModel(ctx, personal, interfaces.AzureModelRecord{
		ID: "chatchat01", ConnectionID: "default", Kind: interfaces.AzureKindClaude, Deployment: "claude-sonnet-4-6",
		DisplayName: "Docs writer", Purpose: interfaces.AzurePurposeCustom, PurposeCustom: "Docs",
		Description: "Writes flow documentation", ContextWindow: 200_000, MaxOutputTokens: 8192,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateModel(ctx, personal, interfaces.AzureModelRecord{
		ID: "embedemb01", ConnectionID: "default", Kind: interfaces.AzureKindEmbedding, Deployment: "text-embedding-3-large",
		DisplayName: "Search index", Purpose: interfaces.AzurePurposeGeneral, IsKnowledgeEmbedding: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordModelTest(ctx, personal, chat.ID, &interfaces.AzureTestResult{OK: true, LatencyMs: 800}); err != nil {
		t.Fatal(err)
	}
	marker, err := a.MarkImported(ctx, personal)
	if err != nil {
		t.Fatal(err)
	}

	b := newFileRegistry(t, dir)
	wantConns, _ := a.ListConnections(ctx, personal)
	gotConns, _ := b.ListConnections(ctx, personal)
	wantModels, _ := a.ListModels(ctx, personal)
	gotModels, _ := b.ListModels(ctx, personal)
	if fmt.Sprintf("%+v", deref(gotConns)) != fmt.Sprintf("%+v", deref(wantConns)) {
		t.Errorf("resources after a restart:\n got %+v\nwant %+v", deref(gotConns), deref(wantConns))
	}
	if fmt.Sprintf("%+v", derefModels(gotModels)) != fmt.Sprintf("%+v", derefModels(wantModels)) {
		t.Errorf("models after a restart:\n got %+v\nwant %+v", derefModels(gotModels), derefModels(wantModels))
	}
	if at, ok, _ := b.ImportedAt(ctx, personal); !ok || !at.Equal(marker) {
		t.Errorf("import marker after a restart = (%v, %v), want %v", at, ok, marker)
	}
}

// deref flattens the pointer fields so %+v compares values.
func deref(conns []interfaces.AzureConnectionRecord) []string {
	out := make([]string, 0, len(conns))
	for _, c := range conns {
		test, dismissed := "", ""
		if c.LastTest != nil {
			test = fmt.Sprintf("%+v", *c.LastTest)
		}
		if c.DiscoveryDismissedAt != nil {
			dismissed = c.DiscoveryDismissedAt.UTC().String()
		}
		c.LastTest, c.DiscoveryDismissedAt = nil, nil
		c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
		out = append(out, fmt.Sprintf("%+v test=%s dismissed=%s", c, test, dismissed))
	}
	return out
}

func derefModels(models []interfaces.AzureModelRecord) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		test := ""
		if m.LastTest != nil {
			test = fmt.Sprintf("%+v", *m.LastTest)
		}
		m.LastTest = nil
		m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
		out = append(out, fmt.Sprintf("%+v test=%s", m, test))
	}
	return out
}

// TestFileAzureRegistry_OwnFileOnly: the store writes azure-models.json
// (mode 0600) and never touches settings.json beside it.
func TestFileAzureRegistry_OwnFileOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	const settingsBody = `{"version": 1, "theme": "dark"}`
	if err := os.WriteFile(settings, []byte(settingsBody), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(settings)
	s := newFileRegistry(t, dir)
	if s.Path() != filepath.Join(dir, AzureRegistryFileName) {
		t.Errorf("Path = %q", s.Path())
	}
	if _, err := s.CreateConnection(ctx, personal, interfaces.AzureConnectionRecord{ID: "default", Name: "res"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkImported(ctx, personal); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(settings)
	if data, _ := os.ReadFile(settings); err != nil || string(data) != settingsBody || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("settings.json was touched (err %v)", err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("the registry file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("registry file mode = %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != AzureRegistryFileName+",settings.json" {
		t.Errorf("directory holds %v, want only the registry file and settings.json", names)
	}
}

// TestFileAzureRegistry_NoopWritesDoNotRewrite: an import that skips (the
// row exists) or a repeated import marker changes nothing and must not bump
// the revision — a needless rewrite would make another process's write
// conflict for no reason.
func TestFileAzureRegistry_NoopWritesDoNotRewrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := newFileRegistry(t, dir)
	rec := interfaces.AzureConnectionRecord{ID: "default", Name: "res"}
	if created, err := s.ImportConnection(ctx, personal, rec); err != nil || !created {
		t.Fatalf("import = (%v, %v)", created, err)
	}
	if _, err := s.MarkImported(ctx, personal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDiscoveryDismissed(ctx, personal, "default", true); err != nil {
		t.Fatal(err)
	}
	rev := fileRevision(t, dir)
	if created, err := s.ImportConnection(ctx, personal, rec); err != nil || created {
		t.Fatalf("re-import = (%v, %v), want skipped", created, err)
	}
	if _, err := s.MarkImported(ctx, personal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDiscoveryDismissed(ctx, personal, "default", true); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeConnection(ctx, personal, "default"); err != nil {
		t.Fatal(err)
	}
	if got := fileRevision(t, dir); got != rev {
		t.Errorf("revision moved from %d to %d on writes that changed nothing", rev, got)
	}
	if _, err := s.SetDiscoveryDismissed(ctx, personal, "default", false); err != nil {
		t.Fatal(err)
	}
	if got := fileRevision(t, dir); got != rev+1 {
		t.Errorf("revision = %d after a real change, want %d", got, rev+1)
	}
}

// TestFileAzureRegistry_EmptyConfigDirRefused guards the constructor.
func TestFileAzureRegistry_EmptyConfigDirRefused(t *testing.T) {
	if _, err := NewFileAzureRegistry(""); err == nil {
		t.Error("NewFileAzureRegistry(\"\") must fail rather than write to the working directory")
	}
}
