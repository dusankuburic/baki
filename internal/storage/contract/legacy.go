package contract

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// OpenLegacySource opens a migration.LegacySource over a data directory the suite has laid out.
type OpenLegacySource func(t *testing.T, dataDir string) migration.LegacySource

// RunLegacySourceSuite runs the read-only contract of the migrator's source. Every fixture is written as raw files
// in the layout old filesystem-backed builds left behind (LegacyDir), never through a writer of this build, so the
// suite pins what the migrator can read, not what today's writer happens to produce.
func RunLegacySourceSuite(t *testing.T, open OpenLegacySource) {
	t.Helper()
	for _, sc := range legacyScenarios {
		t.Run(sc.name, func(t *testing.T) { sc.run(t, open) })
	}
}

var legacyScenarios = []struct {
	name string
	run  func(t *testing.T, open OpenLegacySource)
}{
	{"LoadFlow_missing_returns_ErrNotFound", legacyMissingFlow},
	{"LoadFlow_reads_the_old_layout", legacyLoadFlow},
	{"LoadFlow_returns_the_flow_under_its_file_name", legacyFlowIdentity},
	{"ListFlowIDs_pages_every_flow_once_in_ID_order", legacyListWalk},
	{"ListFlowIDs_lists_an_undecodable_flow_and_LoadFlow_reports_it", legacyCorruptFlow},
	{"LoadSettings_reads_settings_json", legacyLoadSettings},
	{"LoadSettings_missing_returns_ErrNotFound", legacyMissingSettings},
	{"Conversations_read_in_both_old_shapes", legacyConversations},
	{"Conversations_none_stored_lists_nothing", legacyNoConversations},
	{"Conversation_malformed_fails_on_load_only", legacyMalformedConversation},
	{"Path_traversal_is_refused", legacyTraversal},
	{"Reading_changes_nothing_on_disk", legacyReadOnly},
}

// LegacyDir lays out a legacy data directory as old filesystem-backed builds wrote it: flows/<id>.json (the
// indented flow document), settings.json, and conversations/<scope>/<flowID>.json (the chat service's file, or the
// bare message list the filesystem backend saved).
type LegacyDir struct {
	t    testing.TB
	Root string
}

// NewLegacyDir returns an empty legacy data directory in a test temp dir.
func NewLegacyDir(t testing.TB) *LegacyDir {
	t.Helper()
	return &LegacyDir{t: t, Root: t.TempDir()}
}

// File writes data at the slash-separated path rel under the root and returns the file's path.
func (d *LegacyDir) File(rel string, data []byte) string {
	d.t.Helper()
	p := filepath.Join(d.Root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		d.t.Fatal(err)
	}
	return p
}

// Flow writes flows/<doc.ID>.json.
func (d *LegacyDir) Flow(doc *interfaces.FlowDocument) {
	d.t.Helper()
	d.File("flows/"+doc.ID+".json", d.indent(doc))
}

// Settings writes settings.json.
func (d *LegacyDir) Settings(s *interfaces.AppSettings) {
	d.t.Helper()
	d.File("settings.json", d.indent(s))
}

// Conversation writes conversations/<scope>/<flowID>.json in the chat service's file shape.
func (d *LegacyDir) Conversation(scope, flowID string, msgs ...models.ChatMessage) {
	d.t.Helper()
	d.File("conversations/"+scope+"/"+flowID+".json", d.indent(models.ConversationFile{
		Version: 1, FlowKey: flowID, Scope: scope, UpdatedAt: time.Now(), Messages: msgs,
	}))
}

func (d *LegacyDir) indent(v any) []byte {
	d.t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		d.t.Fatal(err)
	}
	return data
}

// MakeReadOnly clears the write bits of everything under the root, restoring them when the test ends, and reports
// whether the OS now refuses to create a file there: Windows ignores the bit on directories, and root ignores it.
func (d *LegacyDir) MakeReadOnly() bool {
	d.t.Helper()
	modes := map[string]fs.FileMode{}
	err := filepath.WalkDir(d.Root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		modes[p] = info.Mode().Perm()
		return nil
	})
	if err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(func() {
		for p, mode := range modes {
			_ = os.Chmod(p, mode)
		}
	})
	for p, mode := range modes {
		if err := os.Chmod(p, mode&^0o222); err != nil {
			d.t.Fatal(err)
		}
	}
	probe := filepath.Join(d.Root, ".write-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return true
	}
	if err := os.Remove(probe); err != nil {
		d.t.Fatal(err)
	}
	return false
}

// Snapshot records every entry under the root (path, mode, size, modification time and content digest), so a
// test can prove a reader changed nothing.
func (d *LegacyDir) Snapshot() map[string]string {
	d.t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(d.Root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		sum := ""
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(p) // #nosec G304 -- walking the test's own temp dir
			if err != nil {
				return err
			}
			sum = fmt.Sprintf("%x", sha256.Sum256(data))
		}
		rel, _ := filepath.Rel(d.Root, p)
		out[filepath.ToSlash(rel)] = fmt.Sprintf("%v %d %d %s", info.Mode(), info.Size(), info.ModTime().UnixNano(), sum)
		return nil
	})
	if err != nil {
		d.t.Fatal(err)
	}
	return out
}

// legacyFlowFile is a flow exactly as an old build wrote it, keys and all; a renamed JSON tag on FlowDocument
// breaks the scenario that reads it.
const legacyFlowFile = `{
  "id": "legacy-flow-1",
  "name": "Invoice run",
  "description": "nightly batch",
  "content": {"subflows": [{"name": "Main", "actions": 12}]},
  "source": "Display.ShowMessageDialog Message: $'''done'''",
  "metadata": {
    "BlockCount": 12,
    "SubflowCount": 1,
    "MaxDepth": 2,
    "ParsedAt": "2025-03-01T10:00:00Z",
    "FileSize": 2048,
    "RawLineCount": 40
  },
  "ownerId": "user-1",
  "organizationId": "",
  "createdAt": "2025-03-01T09:00:00Z",
  "updatedAt": "2025-03-02T09:00:00+02:00",
  "version": 4
}`

func legacyMissingFlow(t *testing.T, open OpenLegacySource) {
	ctx := context.Background()
	d := NewLegacyDir(t)
	if _, err := open(t, d.Root).LoadFlow(ctx, "no-such-flow"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("no flows directory: LoadFlow = %v, want ErrNotFound", err)
	}
	d.Flow(&interfaces.FlowDocument{ID: "other", Name: "other"})
	if _, err := open(t, d.Root).LoadFlow(ctx, "no-such-flow"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("missing flow: LoadFlow = %v, want ErrNotFound", err)
	}
}

func legacyLoadFlow(t *testing.T, open OpenLegacySource) {
	d := NewLegacyDir(t)
	d.File("flows/legacy-flow-1.json", []byte(legacyFlowFile))
	got, err := open(t, d.Root).LoadFlow(context.Background(), "legacy-flow-1")
	if err != nil {
		t.Fatalf("LoadFlow: %v", err)
	}
	if got.ID != "legacy-flow-1" || got.Name != "Invoice run" || got.Description != "nightly batch" || got.OwnerID != "user-1" || got.Version != 4 {
		t.Errorf("flow fields = %+v", got)
	}
	var content, want any
	_ = json.Unmarshal(got.Content, &content)
	_ = json.Unmarshal([]byte(`{"subflows": [{"name": "Main", "actions": 12}]}`), &want)
	if !reflect.DeepEqual(content, want) {
		t.Errorf("content = %s", got.Content)
	}
	if got.Source != "Display.ShowMessageDialog Message: $'''done'''" {
		t.Errorf("source = %q", got.Source)
	}
	if got.Metadata.BlockCount != 12 || got.Metadata.SubflowCount != 1 || got.Metadata.MaxDepth != 2 || got.Metadata.FileSize != 2048 || got.Metadata.RawLineCount != 40 {
		t.Errorf("metadata = %+v", got.Metadata)
	}
	if !got.CreatedAt.Equal(time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)) || !got.UpdatedAt.Equal(time.Date(2025, 3, 2, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("createdAt/updatedAt = %v / %v", got.CreatedAt, got.UpdatedAt)
	}
}

func legacyFlowIdentity(t *testing.T, open OpenLegacySource) {
	d := NewLegacyDir(t)
	d.File("flows/by-name.json", []byte(`{"id": "body-id", "name": "renamed on disk", "content": {}}`))
	d.File("flows/no-id.json", []byte(`{"name": "written without an id", "content": {}}`))
	src := open(t, d.Root)
	for _, id := range []string{"by-name", "no-id"} {
		got, err := src.LoadFlow(context.Background(), id)
		if err != nil {
			t.Fatalf("LoadFlow(%s): %v", id, err)
		}
		if got.ID != id {
			t.Errorf("LoadFlow(%s).ID = %q, want the file name the listing and a rerun's skip check use", id, got.ID)
		}
	}
}

func legacyListWalk(t *testing.T, open OpenLegacySource) {
	ctx := context.Background()
	d := NewLegacyDir(t)
	if ids, err := open(t, d.Root).ListFlowIDs(ctx, "", 10); err != nil || len(ids) != 0 {
		t.Fatalf("no flows directory: ListFlowIDs = %v, %v; want empty", ids, err)
	}

	// "a-b" sorts before "a" by file name ("a-b.json" < "a.json") but after it by ID.
	want := []string{"Z", "a", "a-b", "a.b", "b"}
	for i := 1; i <= 12; i++ {
		want = append(want, fmt.Sprintf("flow-%02d", i))
	}
	for _, id := range want {
		d.Flow(&interfaces.FlowDocument{ID: id, Name: "flow " + id, Content: json.RawMessage(`{}`)})
	}
	d.File("flows/.baki-tmp-123", []byte("partial write"))
	d.File("flows/readme.txt", []byte("not a flow"))
	d.File("flows/nested.json/inner.json", []byte(`{}`))
	slices.Sort(want)

	src := open(t, d.Root)
	for _, limit := range []int{1, 3, 5, len(want), 100} {
		var got []string
		after := ""
		for page := 0; ; page++ {
			if page > len(want)+1 {
				t.Fatalf("limit %d: the walk does not end", limit)
			}
			ids, err := src.ListFlowIDs(ctx, after, limit)
			if err != nil {
				t.Fatalf("limit %d after %q: %v", limit, after, err)
			}
			if len(ids) > limit {
				t.Fatalf("limit %d: a page of %d", limit, len(ids))
			}
			got = append(got, ids...)
			if len(ids) < limit {
				break
			}
			after = ids[len(ids)-1]
		}
		if !slices.Equal(got, want) {
			t.Errorf("limit %d: walked %v\nwant %v", limit, got, want)
		}
	}
	if ids, err := src.ListFlowIDs(ctx, want[len(want)-1], 10); err != nil || len(ids) != 0 {
		t.Errorf("after the last ID: %v, %v; want empty", ids, err)
	}
}

func legacyCorruptFlow(t *testing.T, open OpenLegacySource) {
	ctx := context.Background()
	d := NewLegacyDir(t)
	d.Flow(&interfaces.FlowDocument{ID: "good", Name: "good", Content: json.RawMessage(`{}`)})
	d.File("flows/corrupt.json", []byte(`{"id": "corrupt", "name": `))
	src := open(t, d.Root)
	ids, err := src.ListFlowIDs(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"corrupt", "good"}) {
		t.Fatalf("ListFlowIDs = %v; an undecodable flow must be listed so the copy reports it", ids)
	}
	if _, err := src.LoadFlow(ctx, "corrupt"); err == nil || errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("LoadFlow(corrupt) = %v, want a decode error", err)
	}
}

func legacyLoadSettings(t *testing.T, open OpenLegacySource) {
	d := NewLegacyDir(t)
	d.File("settings.json", []byte(`{
  "version": 1,
  "general": {"firstRunCompleted": true},
  "appearance": {"theme": "light", "density": "compact", "reduceMotion": false, "highContrast": false},
  "layout": {"sidebarWidth": 300, "inspectorWidth": 320},
  "ai": {"activeProvider": "openai", "providers": {}},
  "parser": {"maxFileSizeMB": 25},
  "analysis": {"rules": {"deep-nesting": {"enabled": false, "severity": "warning"}}, "autoAnalyzeOnOpen": true},
  "recentFiles": [{"path": "C:/flows/a.txt", "name": "a.txt", "size": 10, "lastOpen": "2025-03-01T10:00:00Z", "isFolder": false}]
}`))
	got, err := open(t, d.Root).LoadSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if !got.General.FirstRunCompleted || got.Appearance.Theme != "light" || got.AI.ActiveProvider != "openai" || got.Parser.MaxFileSizeMB != 25 || !got.Analysis.AutoAnalyzeOnOpen {
		t.Errorf("settings = %+v", got)
	}
	if rc, ok := got.Analysis.Rules["deep-nesting"]; !ok || rc.Enabled || rc.Severity != "warning" {
		t.Errorf("rule override = %+v (present %v)", rc, ok)
	}
	if len(got.RecentFiles) != 1 || got.RecentFiles[0].Name != "a.txt" {
		t.Errorf("recent files = %+v", got.RecentFiles)
	}
}

func legacyMissingSettings(t *testing.T, open OpenLegacySource) {
	d := NewLegacyDir(t)
	got, err := open(t, d.Root).LoadSettings(context.Background())
	if !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("LoadSettings = %+v, %v; want ErrNotFound, not defaults the migrator would copy", got, err)
	}
}

func legacyConversations(t *testing.T, open OpenLegacySource) {
	ctx := context.Background()
	d := NewLegacyDir(t)
	cest := time.FixedZone("CEST", 2*60*60)
	d.Conversation("openai", "flow-1",
		models.ChatMessage{ID: "m1", Role: "user", Content: "hello", Timestamp: time.Date(2025, 3, 1, 12, 0, 0, 5, cest)},
		models.ChatMessage{ID: "m2", Role: "assistant", Content: "hi", Model: "m/entry-1", ModelLabel: "GPT house",
			ToolCalls:    []models.ToolCallRecord{{Name: "get_block", Ok: true, DurationMs: 12}},
			FixProposals: []models.FixProposalSnapshot{{ProposalID: "p1", RuleID: "deep-nesting", Status: "applied"}}},
	)
	d.File("conversations/claude/flow-2.json", []byte(`[
  {"id": "s1", "role": "user", "content": "from the filesystem backend", "timestamp": "2025-03-01T10:00:00Z"}
]`))
	d.File("conversations/old-scope/flow-3.json", []byte(`{"version": 1, "flowKey": "flow-3b", "scope": "new-scope", "messages": [{"id": "k", "role": "user", "content": "moved", "timestamp": "2025-03-01T10:00:00Z"}]}`))
	d.File("conversations/empty/flow-4.json", []byte(`{"version": 1, "flowKey": "flow-4", "scope": "empty", "messages": null}`))
	d.File("conversations/loose.json", []byte(`[]`))
	d.File("conversations/openai/nested/deep.json", []byte(`[]`))
	d.File("conversations/openai/.baki-tmp-9", []byte(`[`))
	d.File("conversations/openai/notes.txt", []byte(`x`))

	src := open(t, d.Root)
	refs, err := src.ListConversations(ctx)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	var names []string
	for _, r := range refs {
		names = append(names, r.Scope+"/"+r.FlowID)
		if r.Path == "" {
			t.Errorf("%s/%s: empty Path", r.Scope, r.FlowID)
		}
	}
	if want := []string{"claude/flow-2", "empty/flow-4", "old-scope/flow-3", "openai/flow-1"}; !slices.Equal(names, want) {
		t.Fatalf("ListConversations = %v, want %v", names, want)
	}

	load := func(i int) *migration.LegacyConversation {
		t.Helper()
		c, err := src.LoadConversation(ctx, refs[i])
		if err != nil {
			t.Fatalf("LoadConversation(%s): %v", names[i], err)
		}
		return c
	}
	if c := load(0); c.FlowID != "flow-2" || c.Scope != "claude" || len(c.Messages) != 1 || c.Messages[0].Content != "from the filesystem backend" || c.Messages[0].Timestamp != "2025-03-01T10:00:00Z" {
		t.Errorf("bare list = %+v", c)
	}
	if c := load(1); c.Messages == nil || len(c.Messages) != 0 {
		t.Errorf("no messages = %#v, want a non-nil empty list", c.Messages)
	}
	if c := load(2); c.FlowID != "flow-3b" || c.Scope != "new-scope" {
		t.Errorf("the file's flowKey and scope should win over the path: %+v", c)
	}
	c := load(3)
	if c.FlowID != "flow-1" || c.Scope != "openai" || len(c.Messages) != 2 {
		t.Fatalf("chat service file = %+v", c)
	}
	if got := c.Messages[0].Timestamp; got != "2025-03-01T10:00:00.000000005Z" {
		t.Errorf("timestamp = %q, want UTC RFC 3339 with nanoseconds", got)
	}
	m2 := c.Messages[1]
	if m2.Timestamp != "" || m2.ModelLabel != "GPT house" || m2.Model != "m/entry-1" {
		t.Errorf("second message = %+v", m2)
	}
	if len(m2.ToolCalls) != 1 || m2.ToolCalls[0].Name != "get_block" || len(m2.FixProposals) != 1 || m2.FixProposals[0].Status != "applied" {
		t.Errorf("tool calls and fix proposals must survive: %+v / %+v", m2.ToolCalls, m2.FixProposals)
	}
}

func legacyNoConversations(t *testing.T, open OpenLegacySource) {
	d := NewLegacyDir(t)
	refs, err := open(t, d.Root).ListConversations(context.Background())
	if err != nil || len(refs) != 0 {
		t.Errorf("ListConversations = %v, %v; want empty", refs, err)
	}
}

func legacyMalformedConversation(t *testing.T, open OpenLegacySource) {
	ctx := context.Background()
	d := NewLegacyDir(t)
	d.File("conversations/openai/bad.json", []byte(`{not-json`))
	src := open(t, d.Root)
	refs, err := src.ListConversations(ctx)
	if err != nil || len(refs) != 1 {
		t.Fatalf("ListConversations = %v, %v; want the malformed file listed", refs, err)
	}
	if _, err := src.LoadConversation(ctx, refs[0]); err == nil {
		t.Error("LoadConversation of a malformed file should fail")
	}
}

func legacyTraversal(t *testing.T, open OpenLegacySource) {
	ctx := context.Background()
	d := NewLegacyDir(t)
	d.File("secret.json", []byte(`{"id": "secret", "name": "outside the flows directory", "content": {}}`))
	d.File("conversations/secret.json", []byte(`[{"id": "x", "role": "user", "content": "outside a scope"}]`))
	d.Flow(&interfaces.FlowDocument{ID: "inside", Name: "inside", Content: json.RawMessage(`{}`)})
	d.Conversation("openai", "inside")
	src := open(t, d.Root)

	for _, id := range []string{"../secret", `..\secret`, "..", ".", "", "a/b", `a\b`, filepath.Join(d.Root, "secret")} {
		if got, err := src.LoadFlow(ctx, id); err == nil || got != nil {
			t.Errorf("LoadFlow(%q) = %+v, %v; want refused", id, got, err)
		}
	}
	for _, ref := range []migration.ConversationRef{
		{Scope: "..", FlowID: "secret"},
		{Scope: "openai", FlowID: "../secret"},
		{Scope: "openai", FlowID: `..\..\secret`},
		{Scope: "", FlowID: "secret"},
		{Scope: "openai/..", FlowID: "secret"},
	} {
		if got, err := src.LoadConversation(ctx, ref); err == nil || got != nil {
			t.Errorf("LoadConversation(%+v) = %+v, %v; want refused", ref, got, err)
		}
	}
}

func legacyReadOnly(t *testing.T, open OpenLegacySource) {
	ctx := context.Background()
	d := NewLegacyDir(t)
	for _, id := range []string{"f1", "f2", "f3"} {
		d.Flow(&interfaces.FlowDocument{ID: id, Name: id, Content: json.RawMessage(`{}`)})
	}
	d.File("flows/corrupt.json", []byte(`{`))
	d.Settings(&interfaces.AppSettings{Version: 1})
	d.Conversation("openai", "f1", models.ChatMessage{ID: "m", Role: "user", Content: "hi"})
	// A crash-orphaned temp file old enough for the old backend's startup sweep to remove it.
	stale := d.File("flows/.baki-tmp-stale", []byte("orphan"))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if !d.MakeReadOnly() {
		t.Log("this OS lets the test create files in a read-only directory; the snapshot comparison still proves nothing was written")
	}
	before := d.Snapshot()
	if _, ok := before["flows/.baki-tmp-stale"]; !ok {
		t.Fatal("the fixture has no stale temp file")
	}

	src := open(t, d.Root)
	var loaded int
	after := ""
	for {
		ids, err := src.ListFlowIDs(ctx, after, 2)
		if err != nil {
			t.Fatalf("ListFlowIDs: %v", err)
		}
		for _, id := range ids {
			if _, err := src.LoadFlow(ctx, id); err == nil {
				loaded++
			}
		}
		if len(ids) < 2 {
			break
		}
		after = ids[len(ids)-1]
	}
	if loaded != 3 {
		t.Errorf("loaded %d flows from the read-only directory, want 3", loaded)
	}
	if _, err := src.LoadSettings(ctx); err != nil {
		t.Errorf("LoadSettings: %v", err)
	}
	refs, err := src.ListConversations(ctx)
	if err != nil || len(refs) != 1 {
		t.Fatalf("ListConversations = %v, %v", refs, err)
	}
	if _, err := src.LoadConversation(ctx, refs[0]); err != nil {
		t.Errorf("LoadConversation: %v", err)
	}

	afterRead := d.Snapshot()
	for p, was := range before {
		if now, ok := afterRead[p]; !ok {
			t.Errorf("%s was removed", p)
		} else if now != was {
			t.Errorf("%s changed: %s -> %s", p, was, now)
		}
	}
	for p := range afterRead {
		if _, ok := before[p]; !ok {
			t.Errorf("%s was created", p)
		}
	}
}
