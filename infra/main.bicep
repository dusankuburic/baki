// Azure Container Apps infrastructure for the baki (PAD Analyzer) cloud backend.
//
// Provisions: Log Analytics workspace, Managed Environment, a user-assigned
// managed identity (for Azure Blob Managed-Identity access), and a hardened
// Container App. Deploy resource-group scoped:
//   az deployment group create -g rg-baki-prod -f infra/main.bicep \
//     --parameters @infra/main.parameters.json \
//     --parameters authSecret=<...> databaseUrl=<...>
//
// Severity rationale: single-replica deployment, so
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

@description('Container registry the image is pulled from. GHCR by default (deploy.yml pushes there).')
param registryServer string = 'ghcr.io'

@description('Registry username. GHCR is a private registry for this repo, so ACA needs pull credentials at every revision create AND every restart/scale event — managed identity cannot authenticate to GHCR. Leave both empty only if the package is public.')
param registryUsername string = ''

@secure()
@description('Registry password/token. For GHCR: a classic PAT with read:packages (deploy.yml passes secrets.GHCR_PULL_TOKEN). The workflow GITHUB_TOKEN is NOT usable: it expires when the run ends, and ACA pulls again on every restart.')
param registryPassword string = ''

@secure()
@description('Bearer token that gates GET /metrics. Behind the ACA ingress every request arrives from a private address, so the app\'s private-network allowance is satisfied by everyone; this token is the only real gate. Empty leaves /metrics public (the app logs a warning at boot).')
param metricsToken string = ''

@description('Optional resource ID of an existing storage account. When provided (non-empty), the app uses it and provisionStorage is ignored.')
param storageAccountResourceId string = ''

@description('Provision a dedicated storage account + blob container for flow-content offload when storageAccountResourceId is empty. Default true.')
param provisionStorage bool = true

@description('Provision an Azure Cache for Redis (the shared backplane for rate-limiting, WS presence, chat-resume across replicas). Required when maxReplicas > 1; OFF by default because the default template is single-replica (maxReplicas = 1) and redisx refuses to boot when PAD_REDIS_URL is set but unreachable — so a Redis blip was a hard startup failure for a backplane the deployment did not use. Set true when scaling out.')
param provisionRedis bool = false

@description('Redis SKU.')
param redisSku string = 'Basic'

@description('Redis family (C = Basic/Standard, P = Premium).')
param redisFamily string = 'C'

@description('Redis capacity (0 = 250MB Basic; size up for production.')
param redisCapacity int = 0

@description('Comma-separated email addresses for operational alerts (Action Group recipients). Leave empty to skip alert provisioning.')
param alertEmails string = ''

@description('Comma-separated CORS / WebSocket origin allowlist (PAD_ALLOWED_ORIGINS). The WebSocket upgrade rejects any browser Origin not listed here, so an empty value leaves presence/collaboration dead on the deployed app. Defaults to the Container App\'s own ingress hostname; add your custom domain when you front it with one.')
param allowedOrigins string = ''

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

// ── Storage account (flow-content blob offload) ────────────────────────────
// Provisioned when no existing storageAccountResourceId is supplied AND
// provisionStorage is true. Includes soft-delete + versioning (RPO for
// accidental overwrites/deletes) and a lifecycle policy. The README previously
// listed these as 3 manual `az` CLI steps; provisioning them here makes the
// deploy reproducible.
var storageAccountName = toLower(replace('${appName}storage', '-', ''))

resource storage 'Microsoft.Storage/storageAccounts@2023-05-01' = if (provisionStorage && empty(storageAccountResourceId)) {
  name: storageAccountName
  location: location
  kind: 'StorageV2'
  sku: { name: 'Standard_LRS' }
  properties: {
    accessTier: 'Hot'
    minimumTlsVersion: 'TLS1_2'
    allowBlobPublicAccess: false
    allowSharedKeyAccess: false
    blobProperties: {
      containerDeleteRetentionPolicy: { enabled: true, days: 14 }
      deleteRetentionPolicy: { enabled: true, days: 14 } // blob soft-delete
      isVersioningEnabled: true
      restorePolicy: { enabled: true, days: 13 } // point-in-time restore (<= soft-delete window)
    }
  }
  // A child blob-service lifecycle rule: age out cold blobs to Cool/Archive.
  resource blobService 'blobServices/default' = {
    name: 'default'
    properties: {
      lifecyclePolicies: [
        {
          rules: [
            {
              enabled: true
              name: 'to-cool-then-archive'
              definition: {
                actions: {
                  baseBlob: {
                    tierToCool: { daysAfterModificationGreaterThan: 30 }
                    tierToArchive: { daysAfterModificationGreaterThan: 90 }
                  }
                }
                filters: { blobTypes: ['blockBlob'] }
              }
            }
          ]
        }
      ]
    }
  }
  // The blob container the app writes flow content to.
  resource flowContent 'blobServices/containers' = {
    name: 'default/flow-content'
    properties: { publicAccess: 'None' }
  }
}

// Resolve the effective storage account id (existing or provisioned) + grant
// the MI Storage Blob Data Contributor so the app can read/write blobs via
// Managed Identity (no connection string/secret in the env).
var effectiveStorageId = !empty(storageAccountResourceId) ? storageAccountResourceId : storage.id

// Grant the identity read/write blob access on the storage account used for
// flow content offload. No-op unless storageAccountResourceId is supplied. This
// is scoped at the resource group; deploy into a dedicated RG (or move this
// assignment to the storage account's own scope) to avoid over-broad access.
@description('Storage Blob Data Contributor (built-in role).')
var storageBlobDataContributorRoleId = '2a2b9908-6ea1-4ae2-b09c-2f025f3a3a9a'

resource blobRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(identity.id, effectiveStorageId, storageBlobDataContributorRoleId)
  scope: resourceGroup()
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', storageBlobDataContributorRoleId)
    principalId: identity.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

// ── Azure Cache for Redis (cross-replica backplane) ─────────────────────────
// Powers the shared rate-limit bucket, WS presence/echo, and chat-stream
// resume across replicas. Single-replica deployments (maxReplicas=1) still
// benefit: the app's in-memory fallback keeps working when Redis is briefly
// unreachable (fail-open, documented).
var redisName = toLower(replace('${appName}-cache', '-', ''))

resource redis 'Microsoft.Cache/redis@2023-08-01' = if (provisionRedis) {
  name: redisName
  location: location
  properties: {
    sku: { name: redisSku, family: redisFamily, capacity: redisCapacity }
    enableNonSslPort: false
    minimumTlsVersion: '1.2'
    publicNetworkAccess: 'Enabled' // TODO: restrict to the ACA virtual network in prod
    redisConfiguration: {
      'maxmemory-policy': 'allkeys-lru'
    }
  }
}

// Redis access keys are sensitive; read them at deploy time and surface as a
// Container App secret (never as a plain env var).
var redisPrimaryKey = provisionRedis ? listKeys(redis.id, redis.apiVersion).primaryKey : ''

// Secrets live in Container App secrets (below), sourced from GitHub Actions
// secrets at deploy time. A Key Vault used to be provisioned here as well, but
// nothing ever wrote a secret into it: every boot did six lookups, got six
// 404s, logged six warnings, and fell back to exactly these container secrets.
// Removed rather than finished — ACA secrets already do the job honestly.

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
        // Redis primary key composed into the rediss:// URL below (TLS endpoint).
        { name: 'redis-key', value: redisPrimaryKey }
        { name: 'redis-url', value: provisionRedis ? 'rediss://:${redisPrimaryKey}@${redis.properties.hostName}:6380' : '' }
        { name: 'registry-password', value: registryPassword }
        { name: 'metrics-token', value: metricsToken }
      ]
      // Pull credentials for the private image. This used to be `registries: []`,
      // which meant the FIRST successful deploy would have gone straight to
      // ImagePullBackOff on the private GHCR image — the pipeline had never run
      // far enough to show it.
      registries: empty(registryUsername) ? [] : [
        {
          server: registryServer
          username: registryUsername
          passwordSecretRef: 'registry-password'
        }
      ]
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
            // Origin allowlist for CORS and the /ws upgrade. The app's own
            // ingress hostname is <appName>.<environment default domain>; the
            // resource cannot reference its own fqdn output, so it is composed.
            { name: 'PAD_ALLOWED_ORIGINS', value: !empty(allowedOrigins) ? allowedOrigins : 'https://${appName}.${env.properties.defaultDomain}' }
            // Tell DefaultAzureCredential which managed identity to use.
            { name: 'AZURE_CLIENT_ID', value: identity.properties.clientId }
            { name: 'PAD_AUTH_SECRET', secretRef: 'auth-secret' }
            { name: 'PAD_ENCRYPTION_KEY', secretRef: 'encryption-key' }
            { name: 'PAD_DATABASE_URL', secretRef: 'database-url' }
            // Redis backplane (TLS). Empty when provisionRedis is false (app
            // uses its in-memory fallback). ACA secret interpolation composes
            // the key into the URL without surfacing it as a plain env value.
            { name: 'PAD_REDIS_URL', secretRef: 'redis-url' }
            // Gates /metrics (see metricsToken). Empty = public, with a boot warning.
            { name: 'PAD_METRICS_TOKEN', secretRef: 'metrics-token' }
            // Storage (Managed-Identity auth, no secret): the account name +
            // container the app writes flow-content blobs to.
            { name: 'PAD_AZURE_STORAGE_ACCOUNT', value: provisionStorage && empty(storageAccountResourceId) ? storage.name : '' }
            { name: 'PAD_AZURE_STORAGE_CONTAINER', value: 'flow-content' }
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

// ── Alerting (A3) ───────────────────────────────────────────────────────────
// An Action Group (notification target) + the highest-signal built-in metric
// alert: Container App restarts (crash-loop). The app's ~30 custom Prometheus
// metrics (pad_audit_spill_depth, pad_background_loop_last_tick_timestamp_seconds,
// pad_ai_request_errors_total, etc.) are exported at /metrics and the OTel
// errsink already funnels panics/errors to Application Insights; surfacing them
// as automated Azure-Monitor alerts requires the managed-service-for-Prometheus
// scrape pipeline (a Microsoft.Monitor/accounts + diagnostic-settings wiring),
// which is intentionally NOT included here to keep the first-deploy surface
// small. Add it as a follow-up once the metric-to-alert mapping is confirmed
// against a live deployment.
var actionGroupName = '${appName}-alerts'

var alertEmailList = !empty(alertEmails) ? split(alertEmails, ',') : []

resource actionGroup 'Microsoft.Insights/actionGroups@2023-01-01' = if (!empty(alertEmails)) {
  name: actionGroupName
  location: 'global'
  properties: {
    groupShortName: appName
    enabled: true
    emailReceivers: [
      for (email, i) in alertEmailList: {
        name: 'email-${i}'
        emailAddress: trim(email)
        useCommonAlertSchema: true
      }
    ]
  }
}

// Container App restart count > 3 over 5 minutes: the app crashed and ACA is
// restarting it (crash-loop / OOM-kill / failed readiness). Highest-signal
// single alert for a single-replica deployment.
resource restartAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = if (!empty(alertEmails)) {
  name: '${appName}-restarts'
  location: 'global'
  properties: {
    severity: 1
    enabled: true
    scopes: [containerApp.id]
    evaluationFrequency: 'PT1M'
    windowSize: 'PT5M'
    targetResourceType: 'Microsoft.App/containerApps'
    criteria: {
      allOf: [
        {
          metricNamespace: 'Microsoft.App/containerApps'
          metricName: 'restarts'
          operator: 'GreaterThan'
          threshold: 3
          timeAggregation: 'Total'
          criterionType: 'StaticThresholdCriterion'
        }
      ]
      'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
    }
    actions: [
      {
        actionGroupId: actionGroup.id
      }
    ]
  }
}

// Log-query alerts on the Log Analytics workspace the environment already
// ships console/system logs to. These are the "one alert" the readiness
// review asked for: the app has ~40 Prometheus metrics that nothing scrapes,
// so until a scrape pipeline exists the structured JSON log IS the signal.
//   - error burst: > 20 level=ERROR lines in 15 minutes (the app logs at
//     ERROR only for failures an operator should see; access-log 5xx lines
//     are included, panics are included).
//   - readiness failing: ACA reports readiness probe failures in the system
//     log; > 3 in 10 minutes means the pod is out of rotation and, single-
//     replica, the app is down.
// Both need alertEmails; the workspace is 'workspace' above.
resource errorBurstAlert 'Microsoft.Insights/scheduledQueryRules@2023-03-15-preview' = if (!empty(alertEmails)) {
  name: '${appName}-error-burst'
  location: location
  properties: {
    displayName: '${appName}: ERROR log burst'
    description: 'More than 20 ERROR-level log lines in 15 minutes. Open the workspace and filter ContainerAppConsoleLogs_CL on level ERROR; each line carries request_id and trace_id.'
    severity: 2
    enabled: true
    evaluationFrequency: 'PT5M'
    windowSize: 'PT15M'
    scopes: [workspace.id]
    criteria: {
      allOf: [
        {
          query: 'ContainerAppConsoleLogs_CL | where ContainerAppName_s == "${appName}" | where Log_s contains \'"level":"ERROR"\''
          timeAggregation: 'Count'
          operator: 'GreaterThan'
          threshold: 20
          failingPeriods: {
            numberOfEvaluationPeriods: 1
            minFailingPeriodsToAlert: 1
          }
        }
      ]
    }
    actions: {
      actionGroups: [actionGroup.id]
    }
  }
}

resource readinessAlert 'Microsoft.Insights/scheduledQueryRules@2023-03-15-preview' = if (!empty(alertEmails)) {
  name: '${appName}-readiness-failing'
  location: location
  properties: {
    displayName: '${appName}: readiness probe failing'
    description: 'ACA reported readiness probe failures for the app more than 3 times in 10 minutes. With one replica this means the app is out of rotation: check /readyz (DB, blob, Redis) and the ERROR log.'
    severity: 1
    enabled: true
    evaluationFrequency: 'PT5M'
    windowSize: 'PT10M'
    scopes: [workspace.id]
    criteria: {
      allOf: [
        {
          query: 'ContainerAppSystemLogs_CL | where ContainerAppName_s == "${appName}" | where Log_s contains "readiness" and Log_s contains "fail"'
          timeAggregation: 'Count'
          operator: 'GreaterThan'
          threshold: 3
          failingPeriods: {
            numberOfEvaluationPeriods: 1
            minFailingPeriodsToAlert: 1
          }
        }
      ]
    }
    actions: {
      actionGroups: [actionGroup.id]
    }
  }
}

output fqdn string = containerApp.properties.configuration.ingress.fqdn
output containerAppName string = containerApp.name
output managedIdentityClientId string = identity.properties.clientId
output managedIdentityPrincipalId string = identity.properties.principalId
output storageAccountName string = provisionStorage && empty(storageAccountResourceId) ? storage.name : ''
output redisName string = provisionRedis ? redis.name : ''
