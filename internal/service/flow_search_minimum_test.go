package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// A library search without a run of three letters or digits (under three characters, or punctuation like "---") is
// refused before the store is asked, with a 400 the client can switch on (QUERY_TOO_SHORT) and a message the caller
// may read: the content match cannot use its index for such a term. The refusal is the same where the backend scans instead (no content search), so the API does
// not answer differently by deployment. A blank term still answers no results, and three characters search.
func TestSearchLibrary_RefusesATermUnderThreeCharacters(t *testing.T) {
	for name, wrap := range map[string]func(*testutil.FakeBackend) interfaces.StorageBackend{
		"pushdown": func(b *testutil.FakeBackend) interfaces.StorageBackend { return &filterRecorder{FakeBackend: b} },
		"scan": func(b *testutil.FakeBackend) interfaces.StorageBackend {
			return &unsupportedContentBackend{FakeBackend: b}
		},
	} {
		t.Run(name, func(t *testing.T) {
			backend := testutil.NewFakeBackend()
			seedSearchableFlow(t, backend, "f1", "Flow One")
			store := wrap(backend)
			svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(store), store, nil, nil)
			ctx := context.Background()

			for _, text := range []string{"n", "ne", "  ne  ", "日本", "---", "%%%", "n e"} {
				res, err := svc.SearchLibrary(ctx, "user-1", models.SearchQuery{Text: text, MaxResults: 10})
				ae, ok := apperr.As(err)
				if !ok || ae.Kind != apperr.KindInvalid || ae.Code != "QUERY_TOO_SHORT" {
					t.Fatalf("SearchLibrary(%q) = %+v, %v; want a 400 QUERY_TOO_SHORT", text, res, err)
				}
				if ae.Message == "" || ae.Details["minLength"] != interfaces.MinSearchTermRunes {
					t.Errorf("SearchLibrary(%q): message %q, details %v; want a readable message and minLength", text, ae.Message, ae.Details)
				}
				if !errors.Is(err, interfaces.ErrQueryTooShort) {
					t.Errorf("SearchLibrary(%q): %v does not wrap ErrQueryTooShort", text, err)
				}
			}
			if rec, ok := store.(*filterRecorder); ok && (len(rec.contentSearches) != 0 || len(rec.listFilters) != 0) {
				t.Errorf("a refused term reached the store: %d content searches, %d listings", len(rec.contentSearches), len(rec.listFilters))
			}

			if res, err := svc.SearchLibrary(ctx, "user-1", models.SearchQuery{Text: "  ", MaxResults: 10}); err != nil || len(res.Results) != 0 {
				t.Errorf("blank term = %+v, %v; want no results and no error", res, err)
			}
			if _, err := svc.SearchLibrary(ctx, "user-1", models.SearchQuery{Text: "nee", MaxResults: 10}); err != nil {
				t.Errorf("three characters: %v; want a search", err)
			}
			if res, err := svc.SearchLibrary(ctx, "user-1", models.SearchQuery{Text: "needle", MaxResults: 10}); err != nil || len(res.Results) == 0 {
				t.Errorf("a longer term = %+v, %v; want the needle block", res, err)
			}
		})
	}
}

// refusingContentBackend is a store whose database draws no trigram from any term (the Postgres store under a C
// ctype, or with letters newer than its C library's tables): its content search refuses what the Go rule lets by.
type refusingContentBackend struct {
	*testutil.FakeBackend
	listed int
}

func (r *refusingContentBackend) SearchFlowContents(context.Context, interfaces.FlowFilter, string, int) ([]*interfaces.FlowDocument, error) {
	return nil, interfaces.ErrQueryTooShort
}

func (r *refusingContentBackend) ListFlows(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	r.listed++
	return r.FakeBackend.ListFlows(ctx, filter)
}

// A term the store's database cannot serve is the same 400 as one the Go rule refuses, not a scan of the caller's
// library: the API answers alike whichever side decides.
func TestSearchLibrary_TheStoresRefusalIsQueryTooShort(t *testing.T) {
	backend := testutil.NewFakeBackend()
	seedSearchableFlow(t, backend, "f1", "Flow One")
	store := &refusingContentBackend{FakeBackend: backend}
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(store), store, nil, nil)
	res, err := svc.SearchLibrary(context.Background(), "user-1", models.SearchQuery{Text: "needle", MaxResults: 10})
	if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindInvalid || ae.Code != "QUERY_TOO_SHORT" || !errors.Is(err, interfaces.ErrQueryTooShort) {
		t.Fatalf("SearchLibrary = %+v, %v; want a 400 QUERY_TOO_SHORT wrapping ErrQueryTooShort", res, err)
	}
	if store.listed != 0 {
		t.Errorf("the refused term was searched by a scan (%d listings)", store.listed)
	}
}
