// Azure Database for PostgreSQL — Flexible Server for the baki cloud backend.
//
// Standalone module: deploy separately from main.bicep when you want the database
// itself provisioned/as-code (otherwise pass an existing server's FQDN to
// main.bicep via PAD_DATABASE_URL). The backup policy here is the IaC half of
// PRODUCTION_READINESS.md #12 — geo-redundant backups + a pinned retention
// window so PITR/geo-restore (see docs/DR_RUNBOOK.md) is always available and
// the config is reproducible rather than a portal-only setting.
//
//   az deployment group create -g rg-baki-prod -f infra/postgres.bicep \
//     --parameters serverName=baki-pg aadAdminObjectId=<mi-object-id> \
//                  aadAdminPrincipalName=<mi-name> adminPassword=<strong-secret>
//
// The app connects with Managed Identity (PAD_DATABASE_URL with
// password=managed-identity); the MI is set as the server's AAD administrator.
// After deploy, grant the MI its database login role (Azure-side SQL, once):
//
//   az postgres flexible-server execute ... \
//     --query "SELECT * FROM pgaadauth_create_principal_with_oid('<mi-name>', '<mi-client-id>', '<mi-tenant-id>', true, true);"
//
// See docs/DR_RUNBOOK.md §5 for the application cutover after a restore.

@description('Azure region for the server.')
param location string = resourceGroup().location

@description('Flexible Server name (globally unique).')
param serverName string

@description('PostgreSQL major version. 16 matches the Docker/db images.')
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

@description('Object ID of the user-assigned Managed Identity to set as the AAD administrator (the app connects as this MI).')
param aadAdminObjectId string

@description('Principal name of the AAD admin Managed Identity (its AAD display name).')
param aadAdminPrincipalName string

@description('Local (password) administrator login. Kept as a break-glass account; the app never uses it.')
param adminUser string = 'baki_admin'

@description('Password for the break-glass local administrator. Pass at deploy time, never commit.')
@secure()
param adminPassword string

var serverNameVar = serverName

resource server 'Microsoft.DBforPostgreSQL/flexibleServers@2023-12-01' = {
  name: serverNameVar
  location: location
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
      // DR policy as code (#12): the retention window is the PITR range; the
      // geo-redundant copy enables §4b (geo-restore) of the DR runbook.
      backupRetentionDays: backupRetentionDays
      geoRedundantBackup: geoRedundantBackup ? 'Enabled' : 'Disabled'
    }
    authConfig: {
      activeDirectoryAuth: 'Enabled'
      passwordAuth: 'Enabled' // kept for the break-glass admin; the app uses MI
    }
    highAvailability: {
      mode: 'Disabled' // single-replica DB; HA is a separate cost/complexity decision
    }
    network: {
      // Delegated subnet + private endpoint are operator choices; default to
      // public access so the first deploy is simple. Lock down in prod.
      publicNetworkAccess: 'Enabled'
    }
  }
}

// Set the app's Managed Identity as the AAD administrator so the backend can
// connect with password=managed-identity (Entra token injected per connection).
resource aadAdmin 'Microsoft.DBforPostgreSQL/flexibleServers/administrators@2023-12-01' = {
  parent: server
  name: 'activeDirectory'
  properties: {
    principalType: 'ServicePrincipal'
    principalName: aadAdminPrincipalName
    objectId: aadAdminObjectId
    tenantId: subscription().tenantId
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

output fqdn string = server.properties.fullyQualifiedDomainName
output administratorLogin string = adminUser
output connectionStringTemplate string = 'postgres://${adminUser}@${server.properties.fullyQualifiedDomainName}:5432/${databaseName}?sslmode=require'
output miConnectionString string = 'postgres://${aadAdminPrincipalName}@${server.properties.fullyQualifiedDomainName}:5432/${databaseName}?sslmode=require&password=managed-identity'
