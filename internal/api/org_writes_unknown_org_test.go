package api

import (
	"context"
	"net/http"
	"testing"

	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/pgtest"
)

// TestOrgWrites_UnknownOrganisationWithoutAccounts: local mode with accounts off checks no membership before a policy
// save, so one naming an organisation that does not exist reached the store, whose foreign key (migration 0048; the
// fake mirrors it) refused it, and the save answered 500. It is the 404 an admin route answers for an unknown
// organisation, on the fake and on Postgres.
func TestOrgWrites_UnknownOrganisationWithoutAccounts(t *testing.T) {
	check := func(t *testing.T, backend storageif.StorageBackend) {
		rt := newTestRouter(backend, false)
		rr := doRequest(t, rt, http.MethodPost, "/api/analysis/policy/save", map[string]any{"orgId": "no-such-org", "name": "Gate"})
		checkStatus(t, rr, http.StatusNotFound)
		var body struct{ Code, Message string }
		decodeJSON(t, rr, &body)
		if body.Code != "NOT_FOUND" {
			t.Errorf("error code = %q (%q), want NOT_FOUND", body.Code, body.Message)
		}
	}
	t.Run("fake", func(t *testing.T) { check(t, testutil.NewFakeBackend()) })
	t.Run("postgres", func(t *testing.T) {
		pg, err := storagedb.New(context.Background(), storagedb.DefaultConfig(pgtest.NewDatabase(t, "")))
		if err != nil {
			t.Fatalf("open the test database: %v", err)
		}
		t.Cleanup(func() { pg.Close() })
		check(t, pg)
	})
}
