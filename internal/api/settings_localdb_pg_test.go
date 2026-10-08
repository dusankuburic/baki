package api

import (
	"context"
	"net/http"
	"testing"

	storagedb "pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/testutil/pgtest"
	"pad-core/models"
)

// TestSettings_LocalDatabaseWithoutAccounts: local mode on Postgres with auth off (the local+db profile) has no users
// row for the local user, so saving the settings into user_settings failed its foreign key and answered 500. Without
// accounts the local user's settings are the deployment's, as on the desktop, and no user row is written.
func TestSettings_LocalDatabaseWithoutAccounts(t *testing.T) {
	dsn := pgtest.NewDatabase(t, "")
	ctx := context.Background()
	pg, err := storagedb.New(ctx, storagedb.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { pg.Close() })
	rt := newTestRouter(pg, false)

	want := *models.DefaultSettings()
	want.Appearance.Theme = "light"
	checkStatus(t, doRequest(t, rt, http.MethodPost, "/api/system/settings", want), http.StatusOK)

	rr := doRequest(t, rt, http.MethodGet, "/api/system/settings", nil)
	checkStatus(t, rr, http.StatusOK)
	var got models.AppSettings
	decodeJSON(t, rr, &got)
	if got.Appearance.Theme != "light" {
		t.Errorf("theme after the save = %q, want light", got.Appearance.Theme)
	}
	if deployment, err := moduleOf[*SystemHandler](t, rt).sysSvc.GetSettings(ctx); err != nil || deployment.Appearance.Theme != "light" {
		t.Errorf("deployment settings: %v; want the saved theme", err)
	}
	var rows int
	if err := pg.DB().QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM user_settings) + (SELECT count(*) FROM settings_sections WHERE scope = 'user')`).Scan(&rows); err != nil {
		t.Fatalf("count user settings rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d user settings rows written, want none", rows)
	}
}
