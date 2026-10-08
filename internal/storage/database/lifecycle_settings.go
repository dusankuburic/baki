package database

import (
	"context"

	"pad-analyzer/internal/storage/interfaces"
)

// The settings domain: the deployment's, each user's and each organisation's settings documents.
func init() {
	registerLifecycle(lifecycleDomain{
		name: "settings",
		tables: []tableClaim{
			{table: "user_settings", export: "settings", erase: erasedByCascade, org: orgNotKeyed,
				noPurge: "a user's settings live as long as the account"},
			{table: "org_settings", noExport: "organisation configuration",
				erase: notPersonal, eraseWhy: "organisation configuration", org: orgByCascade,
				noPurge: "an organisation's settings live as long as the organisation"},
			{table: "app_settings", noExport: "deployment configuration",
				erase: notPersonal, eraseWhy: "deployment configuration", org: orgNotKeyed,
				noPurge: "the deployment's single settings row", legacy: "settings"},
			// The per-section copies of the three documents (ADR 0012): a user's and an organisation's rows go with
			// their scope through foreign keys on the generated user and org columns.
			{table: "settings_sections", export: "settings", erase: erasedByCascade, org: orgByCascade,
				noPurge: "a scope's settings live as long as the scope", legacy: "settings"},
		},
		export: []exportSection{
			// A read error fails the export: a bundle without the settings it claims is not complete.
			section("settings", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) (*interfaces.AppSettings, error) {
				return b.LoadUserSettings(ctx, x.userID)
			}, func(out *interfaces.UserDataExport, v *interfaces.AppSettings) { out.Settings = v }),
		},
	})
}
