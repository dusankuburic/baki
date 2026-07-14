// Azure Container Apps infrastructure for the baki (PAD Analyzer) cloud backend.
//
// Provisions: Log Analytics workspace, Managed Environment, a user-assigned
// managed identity (for Azure Blob Managed-Identity access), and a hardened
// Container App. Deploy resource-group scoped:
//   az deployment group create -g rg-baki-prod -f infra/main.bicep \
//     --parameters @infra/main.parameters.json \
//     --parameters authSecret=<...> databaseUrl=<...>
//
// Severity rationale (PRODUCTION_READINESS.md): single-replica deployment, so
// the per-instance rate-limiter / WS presence hub / chat-resume state are
// acceptable. Scale min=max=1 by default — raise maxReplicas only after adding
// the shared backplane the review calls for.
//
// Container hardening (#20): non-root, no privilege escalation, ALL Linux
// capabilities dropped, read-only root filesystem with a tmpfs /tmp for any
// scratch writes. These are enforced here rather than only in the Dockerfile so
// the runtime securityContext is reproducible from code.

@description('Azure region for all resources.')
param location string = resourceGroup().location

@description('Name of the Container App (also used for dependent resource names).')
param appName string

@description('Fully-qualified container image to deploy, e.g. ghcr.io/org/baki-backend@sha256:...')
param image string

@description('Minimum replica count. Keep 1 unless a shared backplane is in place.')
param minReplicas int = 1

@description('Maximum replica count. Keep 1 for single-replica (review items #1-#3).')
param maxReplicas int = 1

@description('CPU cores per replica (ACA minimum is 0.25).')
param cpu string = '0.5'

@description('Memory per replica.')
param memory string = '1.0Gi'

@description('Auth secret (JWT signing key). Becomes a Container App secret; pass at deploy time, never commit.')
@secure()
param authSecret string

@description('Dedicated at-rest encryption key for the provider-key keystore (AES-256-GCM). Separate from the JWT secret so rotating either doesn'\''t affect the other. Defaults to authSecret for backward compatibility.')
@secure()
param encryptionKey string = ''

@description('Postgres connection string (sslmode=verify-full recommended). Becomes a Container App secret.')
@secure()
param databaseUrl string

@description('Optional Azure Key Vault URL. When set (non-empty), the app resolves pad-auth-secret / pad-database-url from Key Vault. Empty string = unused.')
param keyVaultUrl string = ''

@description('Optional resource ID of an existing storage account. When provided, the managed identity is granted Storage Blob Data Contributor for blob offload.')
param storageAccountResourceId string = ''

var deploymentName = '${appName}-env'
var identityName = '${appName}-mi'

// ── Log Analytics (ACA logs) ────────────────────────────────────────────────
var workspaceName = '${appName}-logs'

resource workspace 'Microsoft.OperationalInsights/workspaces@2022-10-01' = {
  name: workspaceName
  location: location
  properties: {
    sku: { name: 'PerGB2018' }
    retentionInDays: 30
  }
}

// ── Managed Environment ────────────────────────────────────────────────────
resource env 'Microsoft.App/managedEnvironments@2023-05-01' = {
  name: deploymentName
  location: location
  properties: {
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: reference(workspace.id, '2022-10-01').customerId
        sharedKey: listKeys(workspace.id, '2022-10-01').primarySharedKey
      }
    }
  }
}

// ── Managed identity for Azure Blob (Managed Identity) ─────────────────────
resource identity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: identityName
  location: location
}

// Grant the identity read/write blob access on the storage account used for
// flow content offload. No-op unless storageAccountResourceId is supplied. This
// is scoped at the resource group; deploy into a dedicated RG (or move this
// assignment to the storage account's own scope) to avoid over-broad access.
@description('Storage Blob Data Contributor (built-in role).')
var storageBlobDataContributorRoleId = '2a2b9908-6ea1-4ae2-b09c-2f025f3a3a9a'

resource blobRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!empty(storageAccountResourceId)) {
  name: guid(identity.id, storageAccountResourceId, storageBlobDataContributorRoleId)
  scope: resourceGroup()
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', storageBlobDataContributorRoleId)
    principalId: identity.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

// ── Container App ──────────────────────────────────────────────────────────
resource containerApp 'Microsoft.App/containerApps@2023-05-01' = {
  name: appName
  location: location
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${identity.id}': {}
    }
  }
  properties: {
    managedEnvironmentId: env.id
    configuration: {
      activeRevisionsMode: 'Single'
      ingress: {
        external: true
        targetPort: 8080
        transport: 'http'
        // ACA terminates TLS and presents a public HTTPS URL; the app runs HTTP
        // behind it (PAD_BEHIND_PROXY=true).
        allowInsecure: false
        traffic: [
          { weight: 100, latestRevision: true }
        ]
      }
      secrets: [
        { name: 'auth-secret', value: authSecret }
        { name: 'encryption-key', value: !empty(encryptionKey) ? encryptionKey : authSecret }
        { name: 'database-url', value: databaseUrl }
      ]
      registries: []
    }
    template: {
      containers: [
        {
          name: appName
          image: image
          resources: {
            cpu: json(cpu)
            memory: memory
          }
          // Hardened runtime (#20): non-root (the image's USER pad:pad already
          // is, but this enforces it), no privilege escalation, all caps
          // dropped, read-only root fs. A tmpfs /tmp covers scratch writes.
          securityContext: {
            runAsNonRoot: true
            allowPrivilegeEscalation: false
            capabilities: {
              drop: ['ALL']
            }
            readOnlyRootFilesystem: true
          }
          env: [
            { name: 'PAD_MODE', value: 'cloud' }
            { name: 'PAD_HOST', value: '0.0.0.0' }
            { name: 'PAD_PORT', value: '8080' }
            { name: 'PAD_AUTH_ENABLED', value: 'true' }
            { name: 'PAD_STORAGE', value: 'database' }
            { name: 'PAD_BEHIND_PROXY', value: 'true' }
            // Tell DefaultAzureCredential which managed identity to use.
            { name: 'AZURE_CLIENT_ID', value: identity.properties.clientId }
            // Empty value = unused (the app treats an empty PAD_KEYVAULT_URL as unset).
            { name: 'PAD_KEYVAULT_URL', value: keyVaultUrl }
            { name: 'PAD_AUTH_SECRET', secretRef: 'auth-secret' }
            { name: 'PAD_ENCRYPTION_KEY', secretRef: 'encryption-key' }
            { name: 'PAD_DATABASE_URL', secretRef: 'database-url' }
          ]
          volumeMounts: [
            { name: 'tmp', mountPath: '/tmp' }
          ]
          probes: [
            { type: 'Liveness', httpGet: { path: '/healthz', port: 8080 } }
            { type: 'Readiness', httpGet: { path: '/readyz', port: 8080 } }
          ]
        }
      ]
      volumes: [
        { name: 'tmp', emptyDir: {} } // tmpfs-backed scratch space (root fs is read-only)
      ]
      scale: {
        minReplicas: minReplicas
        maxReplicas: maxReplicas
      }
    }
  }
}

output fqdn string = containerApp.properties.configuration.ingress.fqdn
output containerAppName string = containerApp.name
output managedIdentityClientId string = identity.properties.clientId
output managedIdentityPrincipalId string = identity.properties.principalId
