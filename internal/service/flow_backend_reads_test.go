package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// Read-count pins for the service side. The storage-side statements are
// pinned in internal/storage/database/perf_statements_test.go.

func newCountingFlowService(t *testing.T) (*FlowService, *loadCountingBackend) {
	t.Helper()
	backend := &loadCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	authz := NewAuthzService(backend, nil)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, authz, nil)
	return svc, backend
}

// TestGetAuthorized_ReadsTheHeaderOnce: the authz check and the document
// cache's version check each read the flow header, so every warm resolve paid
// two header reads. One header now serves both.
func TestGetAuthorized_ReadsTheHeaderOnce(t *testing.T) {
	svc, backend := newCountingFlowService(t)
	seedCloudFlow(t, backend.FakeBackend, "f1") // owner u1
	ctx := context.Background()

	if _, err := svc.GetAuthorized(ctx, "f1", "u1", "viewer"); err != nil {
		t.Fatalf("cold GetAuthorized: %v", err)
	}
	backend.loadFlowCalls, backend.loadFlowHeaderCalls = 0, 0

	doc, err := svc.GetAuthorized(ctx, "f1", "u1", "viewer")
	if err != nil || doc == nil {
		t.Fatalf("warm GetAuthorized: %v", err)
	}
	if backend.loadFlowHeaderCalls != 1 {
		t.Errorf("warm GetAuthorized read the header %d times, want 1 (authz + cache check share it)", backend.loadFlowHeaderCalls)
	}
	if backend.loadFlowCalls != 0 {
		t.Errorf("warm GetAuthorized loaded the content %d times, want 0 (cache hit)", backend.loadFlowCalls)
	}

	// A denied caller still costs only the header, and never the content.
	backend.loadFlowCalls, backend.loadFlowHeaderCalls = 0, 0
	if _, err := svc.GetAuthorized(ctx, "f1", "mallory", "viewer"); err == nil {
		t.Fatal("non-owner was authorized")
	}
	if backend.loadFlowCalls != 0 || backend.loadFlowHeaderCalls != 1 {
		t.Errorf("denied caller: %d header / %d content reads, want 1 / 0", backend.loadFlowHeaderCalls, backend.loadFlowCalls)
	}
}

// TestGetAuthorizedHeader_NoDocumentResolve: the triage write gates on the
// flow and reads its name; it used to resolve the whole document for that.
func TestGetAuthorizedHeader_NoDocumentResolve(t *testing.T) {
	svc, backend := newCountingFlowService(t)
	seedCloudFlow(t, backend.FakeBackend, "f1")
	hdr, err := svc.GetAuthorizedHeader(context.Background(), "f1", "u1", "editor")
	if err != nil {
		t.Fatalf("GetAuthorizedHeader: %v", err)
	}
	if hdr.Name != "Flow f1" || hdr.Content != nil {
		t.Errorf("header = %+v", hdr)
	}
	if backend.loadFlowCalls != 0 {
		t.Errorf("GetAuthorizedHeader loaded the content %d times", backend.loadFlowCalls)
	}
	if _, err := svc.GetAuthorizedHeader(context.Background(), "f1", "mallory", "editor"); err == nil {
		t.Error("non-owner was authorized")
	}
}

func contentHasSourceKey(t *testing.T, content json.RawMessage) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(content, &m); err != nil {
		t.Fatalf("stored content is not a JSON object: %v", err)
	}
	_, ok := m["source"]
	return ok
}

// TestStoredContent_KeepsSourceForContentSearch: the content JSON keeps the
// raw source (alongside flows.source). The library's content search pushdown
// matches source literals in it; a round that dropped it to save space left
// new flows unfindable by any text that appears only in their source.
func TestStoredContent_KeepsSourceForContentSearch(t *testing.T) {
	svc, backend := newCountingFlowService(t)
	ctx := context.Background()
	const raw = "Display.ShowMessage Title: $'''hi'''"

	up := &models.FlowDocument{ID: "up1", Name: "Up", Source: raw, Subflows: []models.Subflow{{ID: "main", Name: "Main"}}}
	if err := svc.SaveUploadedFlow(ctx, up, "u1"); err != nil {
		t.Fatalf("SaveUploadedFlow: %v", err)
	}
	stored := backend.Flows["up1"]
	if !contentHasSourceKey(t, stored.Content) {
		t.Error("SaveUploadedFlow dropped the source from the content JSON")
	}
	if stored.Source != raw {
		t.Errorf("flows.source = %q, want the raw source", stored.Source)
	}
	if up.Source != raw {
		t.Error("SaveUploadedFlow cleared the caller's doc.Source")
	}
	resolved, err := svc.GetAuthorized(ctx, "up1", "u1", "viewer")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Source != raw {
		t.Errorf("resolved doc.Source = %q, want it restored from the column", resolved.Source)
	}

	// The cloud fix/save path (persistCloudDoc) stores the same way.
	updated := &models.FlowDocument{Name: "Up", Subflows: []models.Subflow{{ID: "main", Name: "Main"}}}
	out, err := svc.persistCloudDoc(ctx, resolved, updated, raw+"\n")
	if err != nil {
		t.Fatalf("persistCloudDoc: %v", err)
	}
	stored = backend.Flows["up1"]
	if !contentHasSourceKey(t, stored.Content) {
		t.Error("persistCloudDoc dropped the source from the content JSON")
	}
	if stored.Source != raw+"\n" || out.Source != raw+"\n" {
		t.Errorf("persistCloudDoc source: column %q, returned %q", stored.Source, out.Source)
	}
}

func searchHits(t *testing.T, svc *FlowService, doc *models.FlowDocument, text string) int {
	t.Helper()
	res, err := svc.SearchFlow(doc, models.SearchQuery{Text: text})
	if err != nil {
		t.Fatalf("SearchFlow: %v", err)
	}
	return len(res.Results)
}

// TestSearchIndex_RebuildsForAVersionWrittenElsewhere: the search index cache
// was keyed by flow ID only and dropped only by a same-process write. A write
// on another replica (here: straight to storage, no InvalidateSearchIndex)
// bumped the version, the provider resolved the new document — and search
// kept answering from the old content.
func TestSearchIndex_RebuildsForAVersionWrittenElsewhere(t *testing.T) {
	svc, backend := newCountingFlowService(t)
	seedCloudFlow(t, backend.FakeBackend, "f1") // one block named "Block"
	ctx := context.Background()

	v0, err := svc.GetAuthorized(ctx, "f1", "u1", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if searchHits(t, svc, v0, "Zebra") != 0 {
		t.Fatal("seed already matches")
	}
	if a, b := svc.searchIndexFor(v0), svc.searchIndexFor(v0); a != b {
		t.Error("the same document must reuse its cached index")
	}

	// "Another replica" saves a new version with a renamed block.
	next := &models.FlowDocument{ID: "f1", Name: "Flow f1", Subflows: []models.Subflow{{
		ID: "f1-sf", Name: "Main",
		Blocks: []models.Block{{ID: "f1-b1", Name: "Zebra", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Children: []models.Block{}}},
	}}}
	content, _ := json.Marshal(next)
	if err := backend.SaveFlow(ctx, &interfaces.FlowDocument{ID: "f1", Name: "Flow f1", Content: content, OwnerID: "u1"}); err != nil {
		t.Fatal(err)
	}

	v1, err := svc.GetAuthorized(ctx, "f1", "u1", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v0 {
		t.Fatal("provider served the stale document")
	}
	if searchHits(t, svc, v1, "Zebra") == 0 {
		t.Error("search answered from the previous version's index")
	}
}

// TestLibraryMetaReads_NoContent: the library detail GET and the delete path
// need metadata only; both loaded the whole document (and its blob).
func TestLibraryMetaReads_NoContent(t *testing.T) {
	backend := &loadCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	seedCloudFlow(t, backend.FakeBackend, "f1")
	lib := NewLibraryService(backend, nil, NewAuthzService(backend, nil))
	ctx := context.Background()

	doc, err := lib.GetLibraryFlowMetaForUser(ctx, "f1", "u1")
	if err != nil {
		t.Fatalf("GetLibraryFlowMetaForUser: %v", err)
	}
	if doc.Name != "Flow f1" || doc.Content != nil {
		t.Errorf("meta = %+v", doc)
	}
	if _, err := lib.GetLibraryFlowMetaForUser(ctx, "f1", "mallory"); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("non-reader: err = %v, want ErrPermissionDenied", err)
	}
	if err := lib.DeleteLibraryFlow(ctx, "f1", "mallory"); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("non-owner delete: err = %v, want ErrPermissionDenied", err)
	}
	if err := lib.DeleteLibraryFlow(ctx, "f1", "u1"); err != nil {
		t.Fatalf("DeleteLibraryFlow: %v", err)
	}
	if backend.loadFlowCalls != 0 {
		t.Errorf("metadata paths loaded the content %d times, want 0", backend.loadFlowCalls)
	}
	if _, ok := backend.Flows["f1"]; ok {
		t.Error("flow not deleted")
	}
}

// scopeBackend serves a fixed id set through ListFlows, honouring AfterID
// (unless ignoreAfterID) and Offset in id order, and records every filter and
// every org-membership lookup.
type scopeBackend struct {
	*testutil.FakeBackend
	ids            []string
	ignoreAfterID  bool
	filters        []interfaces.FlowFilter
	membershipRead int
	rosterRead     int
}

func (b *scopeBackend) ListFlows(_ context.Context, f interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	b.filters = append(b.filters, f)
	var page []*interfaces.FlowDocument
	skipped := 0
	for _, id := range b.ids {
		if !b.ignoreAfterID && f.AfterID != "" && id <= f.AfterID {
			continue
		}
		if skipped < f.Offset {
			skipped++
			continue
		}
		page = append(page, &interfaces.FlowDocument{ID: id})
		if len(page) == f.Limit {
			break
		}
	}
	return page, nil
}

func (b *scopeBackend) ListOrgsForUser(context.Context, string) ([]*interfaces.Organisation, error) {
	b.rosterRead++
	return []*interfaces.Organisation{{ID: "o1", Members: []interfaces.OrgMember{{UserID: "u1"}}}}, nil
}

func (b *scopeBackend) ListOrgMembershipsForUser(context.Context, string) ([]interfaces.OrgMembership, error) {
	b.membershipRead++
	return []interfaces.OrgMembership{{OrgID: "o1", OrgName: "One"}}, nil
}

func newScopeBackend(n int, ignoreAfterID bool) *scopeBackend {
	b := &scopeBackend{FakeBackend: testutil.NewFakeBackend(), ignoreAfterID: ignoreAfterID}
	for i := range n {
		b.ids = append(b.ids, fmt.Sprintf("flow-%05d", i))
	}
	sort.Strings(b.ids)
	return b
}

// TestScopeFlowIDs_KeysetPagingAndOneScopeResolution: the walk paged with
// OFFSET (up to 4500 rows read and discarded on the last page) and rebuilt
// the scope — membership queries included — for every page.
func TestScopeFlowIDs_KeysetPagingAndOneScopeResolution(t *testing.T) {
	b := newScopeBackend(1203, false)
	lib := NewLibraryService(b, nil, NewAuthzService(b, nil))
	ids, orgIDs, err := lib.ScopeFlowIDs(context.Background(), "u1", "", ScopeAll, 0)
	if err != nil {
		t.Fatalf("ScopeFlowIDs: %v", err)
	}
	if len(ids) != len(b.ids) || ids[0] != b.ids[0] || ids[len(ids)-1] != b.ids[len(b.ids)-1] {
		t.Fatalf("got %d ids, want %d", len(ids), len(b.ids))
	}
	if len(orgIDs) != 1 || orgIDs[0] != "o1" {
		t.Errorf("orgIDs = %v", orgIDs)
	}
	if b.membershipRead != 1 || b.rosterRead != 0 {
		t.Errorf("scope resolved %d times via memberships and %d via rosters, want 1 and 0", b.membershipRead, b.rosterRead)
	}
	if len(b.filters) != 3 {
		t.Fatalf("pages = %d, want 3", len(b.filters))
	}
	for i, f := range b.filters {
		if f.Offset != 0 {
			t.Errorf("page %d used OFFSET %d", i, f.Offset)
		}
		want := ""
		if i > 0 {
			want = ids[i*500-1]
		}
		if f.AfterID != want {
			t.Errorf("page %d AfterID = %q, want %q", i, f.AfterID, want)
		}
		if len(f.OrganizationIDs) != 1 {
			t.Errorf("page %d lost the org scope: %v", i, f.OrganizationIDs)
		}
	}
}

// TestScopeFlowIDs_FallsBackToOffsetWhenAfterIDIsIgnored: a backend that
// predates the cursor returns the first page again; the walk must notice and
// finish by OFFSET instead of looping or duplicating ids.
func TestScopeFlowIDs_FallsBackToOffsetWhenAfterIDIsIgnored(t *testing.T) {
	b := newScopeBackend(1100, true)
	lib := NewLibraryService(b, nil, NewAuthzService(b, nil))
	ids, _, err := lib.ScopeFlowIDs(context.Background(), "u1", "", ScopeMine, 0)
	if err != nil {
		t.Fatalf("ScopeFlowIDs: %v", err)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
	if len(ids) != len(b.ids) {
		t.Errorf("got %d ids, want %d", len(ids), len(b.ids))
	}
}
