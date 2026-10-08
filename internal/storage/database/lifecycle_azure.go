package database

import (
	"context"

	"pad-analyzer/internal/storage/interfaces"
)

// The Azure AI Foundry registry: each user's and each organisation's resources and models (their secrets are in the
// keystore).
func init() {
	const orgResources = "the organisation's resources, no user identity"
	registerLifecycle(lifecycleDomain{
		name: "azure registry",
		tables: []tableClaim{
			{table: "user_azure_connections", export: "azureRegistry", erase: erasedByCascade, org: orgNotKeyed,
				noPurge: "a resource lives until its owner removes it"},
			{table: "user_azure_models", export: "azureRegistry", erase: erasedByCascade, org: orgNotKeyed,
				noPurge: "a model lives until its owner removes it"},
			{table: "user_azure_registry_state", noExport: "the one-time import marker",
				erase: erasedByCascade, org: orgNotKeyed, noPurge: "one row per user"},
			{table: "org_azure_connections", noExport: orgResources, erase: notPersonal, eraseWhy: orgResources,
				org: orgByCascade, noPurge: "a resource lives until an admin removes it"},
			{table: "org_azure_models", noExport: orgResources, erase: notPersonal, eraseWhy: orgResources,
				org: orgByCascade, noPurge: "a model lives until an admin removes it"},
			{table: "org_azure_registry_state", noExport: "the one-time import marker", erase: notPersonal,
				eraseWhy: "an organisation's import marker", org: orgByCascade, noPurge: "one row per organisation"},
		},
		export: []exportSection{
			section("azureRegistry", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) (*interfaces.ExportedAzureRegistry, error) {
				return b.exportAzureRegistry(ctx, x.userID)
			}, func(out *interfaces.UserDataExport, v *interfaces.ExportedAzureRegistry) { out.AzureRegistry = v }),
		},
	})
}
