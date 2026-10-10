// Azure Database for PostgreSQL — Flexible Server for the baki cloud backend.
//
// Deploy this BEFORE infra/main.bicep: main.bicep takes the resulting DSN as
// the `databaseUrl` parameter.
//
//   az deployment group create -g rg-baki-prod -f infra/postgres.bicep \
//     --parameters serverName=baki-pg adminPassword=<strong-secret>
//
// Then create the application role and the pgvector extension once, as the
// administrator (see "One-time bootstrap" at the bottom of this file).
//
// The backup policy here is the IaC half of the recovery story — geo-redundant
// backups plus a pinned retention window so PITR and geo-restore are always
// available and reproducible rather than a portal-only setting. The recovery
// procedure, including the separate blob restore it requires, is in README.md.
//
// Connections: pass the `maxConnections` output to infra/main.bicep as
// dbServerMaxConnections, which sizes each replica's pool to fit it. Point the
// app at port 5432, never at the built-in PgBouncer (pgbouncer.enabled, port
// 6432, left off here): it pools in transaction mode, which breaks the session
// advisory locks the app's background jobs hold. docs/architecture/operations.md.

@description('Azure region for the server.')
param location string = resourceGroup().location

@description('Flexible Server name (globally unique).')
param serverName string

@description('Tags applied to the server.')
param tags object = {}

@description('PostgreSQL major version. 16 matches the Docker/CI images.')
param postgresVersion string = '16'

@description('Compute SKU name, e.g. Standard_B1ms (dev) or Standard_D2ds_v5 (prod).')
param skuName string = 'Standard_D2ds_v5'

@allowed([ 'Burstable', 'GeneralPurpose', 'MemoryOptimized' ])
@description('Compute tier. Burstable=B-series, GeneralPurpose=D-series, MemoryOptimized=E/ME-series.')
param skuTier string = 'GeneralPurpose'

@description('Storage size in GiB.')
param storageSizeGB int = 128

@description('Automated backup retention in days (7-35). Sets the PITR window.')
@minValue(7)
@maxValue(35)
param backupRetentionDays int = 14

@description('Enable geo-redundant backups (a paired-region copy for geo-restore).')
param geoRedundantBackup bool = true

@description('App database name (the app uses a single logical database).')
param databaseName string = 'baki'

@description('Administrator login. The application does NOT use this account — create a dedicated NOSUPERUSER NOBYPASSRLS role for it (see the bootstrap notes below).')
param adminUser string = 'baki_admin'

@description('Administrator password. Pass at deploy time, never commit.')
@secure()
param adminPassword string

@description('Allow other Azure services (including the Container Apps environment) to reach the server. REQUIRED on the public-endpoint deployment: publicNetworkAccess=Enabled with zero firewall rules accepts no connections at all, and ACA Consumption has no stable outbound IP to allowlist individually. Turn this off only when you move the server onto a VNet with a private endpoint.')
param allowAzureServices bool = true

@description('Optional extra firewall rules, e.g. an operator workstation: [ { name: "office", startIpAddress: "1.2.3.4", endIpAddress: "1.2.3.4" } ].')
param additionalFirewallRules array = []

@description('PostgreSQL extensions to allow-list via the azure.extensions server parameter. THIS IS LOAD-BEARING: Azure Flexible Server rejects CREATE EXTENSION for anything not listed here, and schema migration v13 (internal/storage/database/postgres_migrations.go) runs an UNGUARDED "CREATE EXTENSION IF NOT EXISTS pg_trgm", so without PG_TRGM the app fails its migrations and never finishes booting. VECTOR backs knowledge-base similarity search; its migration steps ARE exception-guarded, so omitting it degrades search to the in-Go ranker rather than breaking boot.')
param allowedExtensions string = 'VECTOR,PG_TRGM'

@description('Object ID of a Managed Identity or group to set as the Entra administrator. Leave empty to skip — the app uses password auth on this deployment, and an empty value also breaks the otherwise circular dependency with infra/main.bicep (which creates the identity but needs this server\'s DSN).')
param aadAdminObjectId string = ''

@description('Principal (display) name of the Entra administrator. Required when aadAdminObjectId is set.')
param aadAdminPrincipalName string = ''

resource server 'Microsoft.DBforPostgreSQL/flexibleServers@2023-12-01' = {
  name: serverName
  location: location
  tags: tags
  sku: {
    name: skuName
    tier: skuTier
  }
  properties: {
    version: postgresVersion
    administratorLogin: adminUser
    administratorLoginPassword: adminPassword
    storage: {
      storageSizeGB: storageSizeGB
    }
    backup: {
      // DR policy as code: the retention window is the PITR range; the
      // geo-redundant copy enables the geo-restore path in the DR runbook.
      backupRetentionDays: backupRetentionDays
      geoRedundantBackup: geoRedundantBackup ? 'Enabled' : 'Disabled'
    }
    authConfig: {
      activeDirectoryAuth: empty(aadAdminObjectId) ? 'Disabled' : 'Enabled'
      passwordAuth: 'Enabled'
    }
    highAvailability: {
      mode: 'Disabled' // single-instance DB; HA is a separate cost decision
    }
    network: {
      // Public endpoint plus firewall rules. Lock down with a delegated subnet
      // and a private endpoint when the deployment moves onto a VNet.
      publicNetworkAccess: 'Enabled'
    }
  }
}

resource database 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2023-12-01' = {
  parent: server
  name: databaseName
  properties: {
    charset: 'UTF8'
    collation: 'en_US.utf8'
  }
}

// Extension allow-list. Without this the server rejects CREATE EXTENSION with
// 'extension "pg_trgm" is not allow-listed', which fails migration v13 and
// takes the app down on first boot.
resource extensionsConfig 'Microsoft.DBforPostgreSQL/flexibleServers/configurations@2023-12-01' = {
  parent: server
  name: 'azure.extensions'
  properties: {
    value: allowedExtensions
    source: 'user-override'
  }
}

// The special 0.0.0.0-0.0.0.0 rule is Azure's "allow access from Azure
// services" marker, not a literal address range.
resource allowAzure 'Microsoft.DBforPostgreSQL/flexibleServers/firewallRules@2023-12-01' = if (allowAzureServices) {
  parent: server
  name: 'AllowAllAzureServices'
  properties: {
    startIpAddress: '0.0.0.0'
    endIpAddress: '0.0.0.0'
  }
  dependsOn: [ extensionsConfig ] // serialise: concurrent server-level writes conflict
}

@batchSize(1) // Flexible Server refuses concurrent firewall-rule updates
resource extraRules 'Microsoft.DBforPostgreSQL/flexibleServers/firewallRules@2023-12-01' = [
  for rule in additionalFirewallRules: {
    parent: server
    name: rule.name
    properties: {
      startIpAddress: rule.startIpAddress
      endIpAddress: rule.endIpAddress
    }
    dependsOn: [ allowAzure ]
  }
]

// Optional: Entra administrator. Not used by the app on this deployment.
// Note that an Entra administrator is a privileged role — if you later switch
// the app to password=managed-identity, give the app its OWN non-admin
// principal (pgaadauth_create_principal_with_oid with isAdmin=false) rather
// than making the app's identity the server administrator: the app's
// row-level-security model assumes a NOSUPERUSER NOBYPASSRLS role, and
// internal/storage/database/postgres_storage.go warns at boot when it is not.
resource aadAdmin 'Microsoft.DBforPostgreSQL/flexibleServers/administrators@2023-12-01' = if (!empty(aadAdminObjectId)) {
  parent: server
  name: aadAdminObjectId
  properties: {
    principalType: 'ServicePrincipal'
    principalName: aadAdminPrincipalName
    tenantId: subscription().tenantId
  }
  dependsOn: [ allowAzure ]
}

// ── One-time bootstrap (run once, as the administrator) ────────────────────
//
// The app must NOT connect as the administrator: superusers and BYPASSRLS
// roles ignore every row-level-security policy, and migrations also need to
// OWN their tables so that ALTER TABLE ... FORCE ROW LEVEL SECURITY applies.
// This mirrors scripts/db-init/01-app-role.sh, which only runs for the
// Docker Compose stack.
//
//   psql "host=<fqdn> user=<adminUser> dbname=postgres sslmode=require" <<'SQL'
//     CREATE ROLE baki_app LOGIN PASSWORD '<app-password>'
//       NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
//     ALTER DATABASE baki OWNER TO baki_app;
//   SQL
//
//   psql "host=<fqdn> user=<adminUser> dbname=baki sslmode=require" <<'SQL'
//     ALTER SCHEMA public OWNER TO baki_app;
//     GRANT ALL ON SCHEMA public TO baki_app;
//     -- pgvector is an untrusted extension: only an administrator can install
//     -- it, and only after VECTOR is in azure.extensions above. The app's own
//     -- CREATE EXTENSION is best-effort and degrades to the in-Go ranker.
//     CREATE EXTENSION IF NOT EXISTS vector;
//   SQL
//
// Then pass the resulting DSN to infra/main.bicep as `databaseUrl`.

// The SKU's connection cap, read back from the server rather than tabulated here.
resource maxConnectionsConfig 'Microsoft.DBforPostgreSQL/flexibleServers/configurations@2023-12-01' existing = {
  parent: server
  name: 'max_connections'
}

output fqdn string = server.properties.fullyQualifiedDomainName
output serverNameOut string = server.name
output databaseNameOut string = databaseName
output administratorLogin string = adminUser
@description('Template DSN for the DEDICATED application role created in the bootstrap step above — substitute the password. Do not connect the app as the administrator.')
output appConnectionStringTemplate string = 'postgres://baki_app:<app-password>@${server.properties.fullyQualifiedDomainName}:5432/${databaseName}?sslmode=require'
@description('The server\'s max_connections: pass it to infra/main.bicep as dbServerMaxConnections.')
output maxConnections int = int(maxConnectionsConfig.properties.value)
