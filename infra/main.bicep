// Azure Container Apps infrastructure for the baki (PAD Analyzer) cloud backend.
//
// Provisions: Log Analytics workspace, Application Insights, a Managed
// Environment with the managed OpenTelemetry agent, a user-assigned managed
// identity (Blob + ACR pull), an optional storage account for flow-content
// offload, an optional Redis backplane, and a hardened Container App.
//
// Deploy order (resource-group scoped):
//   1. infra/registry.bicep  — the ACR, once. The image must exist before step 3.
//   2. infra/postgres.bicep  — the Flexible Server, once.
//   3. this template:
//      az deployment group create -g rg-baki-prod -f infra/main.bicep \
//        --parameters @infra/main.parameters.json \
//        --parameters authSecret=<...> databaseUrl=<...>
//
// SINGLE REPLICA. minReplicas = maxReplicas = 1 is not a cost default, it is a
// correctness bound: internal/api/events.go is an in-process SSE hub with no
// Redis backplane, so with more than one replica a chat stream started on
// replica A never reaches an SSE client on replica B, and the subscriber
// watchdog in internal/service/chat_resume.go cancels the stream for lack of a
// local subscriber. Cancel-stream, AI budget accounting, the WS ticket replay
// guard and the retention purge are per-process too. Raise maxReplicas only
// after internal/api/events.go grows the backplane that
// internal/websocket/backplane.go already demonstrates.
//
// Note on container hardening: this template used to set a `securityContext`
// block (runAsNonRoot / cap-drop / readOnlyRootFilesystem). That property does
// not exist on Microsoft.App/containerApps at ANY api-version — it appears
// nowhere in the resource's API change log — so it was silently dropped and the
// guarantee was never real. Hardening comes from the image: distroless static,
// USER nonroot (uid 65532), CGO_ENABLED=0. See Dockerfile.

@description('Azure region for all resources.')
param location string = resourceGroup().location

@description('Name of the Container App (also used for dependent resource names).')
param appName string

@description('Fully-qualified container image to deploy, e.g. myacr.azurecr.io/baki-backend@sha256:...')
param image string

@description('Tags applied to every resource this template creates.')
param tags object = {}

@description('Minimum replica count. Keep 1 — see the single-replica note at the top of this file.')
param minReplicas int = 1

@description('Maximum replica count. Keep 1 until internal/api/events.go has a cross-replica backplane.')
param maxReplicas int = 1

// cpu/memory must be one of ACA's allowed pairs (Consumption: 0.25/0.5Gi,
// 0.5/1.0Gi, 0.75/1.5Gi, 1.0/2.0Gi, 1.25/2.5Gi, ... 4.0/8.0Gi — memory is
// always 2 x cpu in Gi). Not 0.5/1.0Gi: Go 1.25 floors GOMAXPROCS at 2 even
// when the CPU quota is below that, so with half a core every CPU-bound
// request (analysis, PDF export, brotli) ran two runtime threads against a
// quota that allowed one and was CFS-throttled for the rest of each 100 ms
// period. 1.0 is the smallest allowed value that does not leave the runtime
// permanently over its quota. GOMEMLIMIT below is derived from `memory`, so
// changing the pair keeps the GC limit consistent automatically.
@description('CPU cores per replica (ACA minimum is 0.25; must pair with memory, see the comment above).')
param cpu string = '1.0'

@description('Memory per replica (must pair with cpu, see the comment above).')
param memory string = '2.0Gi'

// ── Secrets ────────────────────────────────────────────────────────────────

@description('Auth secret (JWT signing key). Becomes a Container App secret; pass at deploy time, never commit.')
@secure()
param authSecret string

@description('Dedicated at-rest encryption key for the provider-key keystore (AES-256-GCM). Separate from the JWT secret so that rotating one does not affect the other. Empty means the app falls back to authSecret and warns at boot. Set it before users store provider keys. To change it later, move the old value to encryptionKeyPrevious (key rotation); replacing it outright makes every stored secret unreadable.')
@secure()
param encryptionKey string = ''

@description('Key rotation: the keys encryptionKey replaced, comma-separated, newest first (PAD_ENCRYPTION_KEY_PREVIOUS). Values they sealed stay readable and are re-sealed with encryptionKey at boot; remove them once the log reports "secret re-seal complete". Needs encryptionKey. Empty when no rotation is under way.')
@secure()
param encryptionKeyPrevious string = ''

@description('Postgres connection string (sslmode=require or verify-full). Becomes a Container App secret.')
@secure()
param databaseUrl string

@secure()
@description('Bearer token that gates GET /metrics and /debug/pprof. Behind the ACA ingress every request arrives from a private address, so the app\'s private-network allowance is satisfied by everyone; this token is the only real gate. Empty leaves /metrics public (the app logs a warning at boot).')
param metricsToken string = ''

@secure()
@description('Deprecated: a deployment-wide HMAC key for POST /api/integrations/ci that reads the whole flow library. Leave it empty and have organisation admins mint CI credentials (ADR 0019); while set, the app warns at boot and marks every answer to it deprecated.')
param ciWebhookSecret string = ''

@secure()
@description('SMTP password. Required together with smtpHost/smtpUsername for transactional email.')
param smtpPassword string = ''

@secure()
@description('OIDC client secret. Optional for PKCE-only public clients.')
param ssoClientSecret string = ''

// ── Container registry ─────────────────────────────────────────────────────

@description('Name of the Azure Container Registry to pull from, in THIS resource group. The Container App authenticates with its user-assigned managed identity (AcrPull), so no registry password is stored anywhere. Leave empty only if the image lives in a public registry. Provision the registry with infra/registry.bicep first — the image must be pushed before this template runs.')
param registryName string = ''

// ── Storage ────────────────────────────────────────────────────────────────

@description('Resource ID of an EXISTING storage account to use for flow-content offload. When provided, provisionStorage is ignored and this template does NOT create the blob role assignment (it cannot scope one outside this resource group) — grant the managed identity "Storage Blob Data Contributor" on that account yourself, and create the container.')
param storageAccountResourceId string = ''

@description('Provision a dedicated storage account + blob container for flow-content offload when storageAccountResourceId is empty.')
param provisionStorage bool = true

@description('Blob container the app writes flow content to. Must match on a bring-your-own account.')
param blobContainerName string = 'flow-content'

@description('Blob soft-delete / container soft-delete / point-in-time-restore window, in days. Change feed is enabled because restorePolicy requires it. Minimum 2: the point-in-time-restore window must be strictly shorter than the soft-delete window, so a value of 1 would leave it at 0.')
@minValue(2)
@maxValue(365)
param blobRetentionDays int = 14

// ── Redis (off by default; single-replica deployment) ──────────────────────

@description('Provision an Azure Cache for Redis (the shared backplane for rate-limiting, WS presence and chat-resume across replicas). OFF by default because this template is single-replica AND the app refuses to boot when PAD_REDIS_URL is set but unreachable — a Redis blip would be a hard startup failure for a backplane the deployment does not use.')
param provisionRedis bool = false

@description('Redis SKU.')
param redisSku string = 'Basic'

@description('Redis family (C = Basic/Standard, P = Premium).')
param redisFamily string = 'C'

@description('Redis capacity (0 = 250MB Basic; size up for production).')
param redisCapacity int = 0

// ── Request handling / proxy ───────────────────────────────────────────────

@description('Comma-separated CORS / WebSocket origin allowlist (PAD_ALLOWED_ORIGINS). The WebSocket upgrade rejects any browser Origin not listed here. Defaults to the Container App\'s own ingress hostname; add your custom domain when you front it with one.')
param allowedOrigins string = ''

@description('Comma-separated proxy IPs/CIDRs whose X-Forwarded-For is trusted (PAD_TRUSTED_PROXIES). THIS MATTERS: with an empty list the app falls back to the immediate TCP peer, which behind the ACA ingress is the ingress itself — so every client collapses into ONE per-IP rate-limit bucket (1 rps general, 5 logins/minute for the whole deployment) and every audit entry records the proxy address. The default covers the ranges the app already treats as in-cluster; confirm the real peer from an access-log line after the first deploy and narrow it.')
param trustedProxies string = '100.64.0.0/10,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16'

@description('Per-request deadline (PAD_REQUEST_TIMEOUT). Empty uses the app default of 30s. /ws and /api/events are exempt; */stream routes get a fixed 15-minute deadline.')
param requestTimeout string = ''

@description('Log level (PAD_LOG_LEVEL): debug, info, warn, error. Empty uses the cloud default of info.')
param logLevel string = ''

@description('Which route policy decides (PAD_ROUTE_POLICY, docs/adr/0006): legacy (the path tables), shadow (the tables decide and pad_route_policy_mismatch_total counts every disagreement with the declared policy) or declarative (the declared policy decides). Empty uses the app default, shadow. Flip to declarative once a soak shows no mismatches; set it here so a later template deploy keeps the flip.')
@allowed([
  ''
  'legacy'
  'shadow'
  'declarative'
])
param routePolicy string = ''

@description('Max open Postgres connections PER REPLICA (PAD_DB_MAX_OPEN_CONNS). Wins over the derivation from dbServerMaxConnections. Empty uses that derivation when dbServerMaxConnections is set, else the app default of 25. Right-size against the Flexible Server SKU\'s connection cap — a Burstable B1ms tops out around 35.')
param dbMaxOpenConns string = ''

@description('The Flexible Server\'s max_connections (SHOW max_connections; infra/postgres.bicep outputs it as maxConnections), e.g. 50 on Burstable B1ms, 859 on General Purpose D2ds_v5. When set and dbMaxOpenConns is empty, each replica\'s pool is (this - dbReservedConnections) / (maxReplicas + max(minReplicas, 1)), at least 1 and at most the app default of 25: the second term is the rolling deploy, whose new revision starts its minReplicas beside the old replicas until the swap (maxReplicas + 1 by default). Also passed to the app (PAD_DB_SERVER_MAX_CONNS). 0 = unknown: the pool stays at the app default. See docs/architecture/operations.md.')
@minValue(0)
param dbServerMaxConnections int = 0

@description('Connections of dbServerMaxConnections the app\'s pools leave alone: the 15 Azure Flexible Server reserves (replication and monitoring), plus 5 for an operator\'s psql or bakicli session. Only used when dbServerMaxConnections is set.')
@minValue(0)
param dbReservedConnections int = 20

// ── Email, SSO, governance ─────────────────────────────────────────────────

@description('Public origin used to build links in transactional email (PAD_APP_BASE_URL), e.g. https://baki.example.com. Defaults to the ingress FQDN when empty.')
param appBaseUrl string = ''

@description('SMTP relay host. Without SMTP the app uses a log-only mailer, which means password-reset and email-verification links are written into Log Analytics instead of being delivered.')
param smtpHost string = ''

@description('SMTP port (587 STARTTLS or 465 TLS).')
param smtpPort string = '587'

@description('SMTP username.')
param smtpUsername string = ''

@description('From address for outbound email. Email is enabled when this and smtpHost are both set.')
param emailFrom string = ''

@description('OIDC issuer URL. For Microsoft Entra ID: https://login.microsoftonline.com/{tenant-id}/v2.0. SSO turns on only when issuer, client ID and redirect URL are ALL set.')
param ssoIssuer string = ''

@description('OAuth2 client ID.')
param ssoClientId string = ''

@description('Absolute callback URL registered with the IdP, e.g. https://baki.example.com/api/auth/sso/callback.')
param ssoRedirectUrl string = ''

@description('Display label and identity-link key for the SSO provider. Changing it after users have linked accounts breaks their links.')
param ssoProviderName string = ''

@description('Periodic library re-scan interval (PAD_SCAN_INTERVAL), e.g. 1h. Empty disables drift/regression alerting.')
param scanInterval string = ''

@secure()
@description('Microsoft Teams incoming-webhook URL for governance alerts. The URL is the credential (anyone holding it can post to the channel), so it is a secret.')
param notifyTeamsUrl string = ''

@secure()
@description('Microsoft Teams Workflows webhook URL for governance alerts (PAD_NOTIFY_TEAMS_WORKFLOW_URL), the replacement of the retiring incoming webhooks. Its sig parameter is the credential, so it is a secret.')
param notifyTeamsWorkflowUrl string = ''

@secure()
@description('Generic JSON webhook URL for governance alerts. A secret: receivers commonly put a token in the path or query.')
param notifyWebhookUrl string = ''

@description('Hosts organisation notification channels may point at, comma-separated: exact hosts or *.suffix globs (PAD_NOTIFY_ALLOWED_HOSTS). Checked when a channel is saved and at every delivery; the deployment channels are not restricted. Empty allows any public host.')
param notifyAllowedHosts string = ''

@description('Age-based purge window for audit events, analysis history, usage metrics and governance alerts (PAD_AUDIT_RETENTION_DAYS). Empty keeps the app default of 0 = forever, which grows Postgres without bound.')
param auditRetentionDays string = ''

@description('On-disk spill queue for audit events when the in-memory pool overflows (PAD_AUDIT_SPILL_DIR). Defaults to /tmp, which on ACA is an ephemeral EmptyDir — spilled events do not survive a restart. Set "off" to drop the spill instead of pretending it is durable.')
param auditSpillDir string = '/tmp/baki-audit-spill'

@description('Block self-registration (PAD_FEATURE_DISABLE_SIGNUP). Note the FIRST registered user becomes admin, so leave this false until that account exists.')
param disableSignUp bool = false

@description('Extra hosts allowed for Azure AI Foundry endpoints, comma-separated: exact hosts or *.suffix globs such as an API Management front (PAD_AZURE_FOUNDRY_ALLOWED_HOSTS). A glob over a public multi-tenant Azure zone (*.azure-api.net, *.azurewebsites.net, ...) refuses to boot — list exact hosts there. Empty uses the built-in allow-list.')
param azureFoundryAllowedHosts string = ''

@description('Allow AI endpoints that resolve to private addresses (PAD_AI_ALLOW_PRIVATE_EGRESS): private-link Foundry resources whose hosts resolve to private addresses by design. Needs VNet integration, which this template does NOT provision — without it a private-link endpoint stays unreachable. Opens RFC 1918 and IPv6 unique-local addresses only; loopback, link-local and metadata addresses stay refused.')
param aiAllowPrivateEgress bool = false

@description('Knowledge-base embedding provider for the whole deployment (PAD_EMBEDDING_PROVIDER): one of openai, gemini, glm, github-models, azure-foundry — anything else refuses to boot. azure-foundry is the only way the knowledge base embeds with Azure AI Foundry; keep every member\'s embedding deployment on the same model. Empty uses the Embedding Assistant setting (default openai).')
param embeddingProvider string = ''

@description('Knowledge-base embedding width (PAD_EMBEDDING_DIM). Empty uses the app default of 1536. Changing it on a populated knowledge base needs a re-index.')
param embeddingDim string = ''

// ── Probes and alerting ────────────────────────────────────────────────────

@description('Seconds before the first probe. A cold start runs schema migrations before it can serve, so do not set this too low.')
@minValue(1)
@maxValue(60)
param probeInitialDelaySeconds int = 10

@description('Probe interval in seconds.')
@minValue(1)
@maxValue(240)
param probePeriodSeconds int = 10

@description('Consecutive readiness failures before the replica leaves rotation.')
@minValue(1)
@maxValue(10)
param readinessFailureThreshold int = 6

@description('Startup probe interval in seconds. Liveness and readiness only begin once the startup probe passes, so slow boot-time schema migrations are not mistaken for a hung container.')
@minValue(1)
@maxValue(240)
param startupProbePeriodSeconds int = 30

@description('Consecutive startup-probe failures before the container is restarted. The boot window is about probeInitialDelaySeconds + startupProbePeriodSeconds x this value (5 minutes by default).')
@minValue(1)
@maxValue(10)
param startupFailureThreshold int = 10

@description('Comma-separated email addresses for operational alerts (Action Group recipients). Leave empty to skip alert provisioning — note that skips EVERY alert below.')
param alertEmails string = ''

// ── Computed ───────────────────────────────────────────────────────────────

var environmentName = '${appName}-env'
var identityName = '${appName}-mi'
var workspaceName = '${appName}-logs'
var appInsightsName = '${appName}-insights'
var storageAccountName = toLower(replace('${appName}storage', '-', ''))
var redisName = toLower(replace('${appName}-cache', '-', ''))
var actionGroupName = '${appName}-alerts'

var provisioningStorage = provisionStorage && empty(storageAccountResourceId)

// Blob offload needs BOTH the account name and the container, or neither:
// internal/config/loader.go refuses to boot on a half-configured pair. The old
// template set the container unconditionally and blanked the account whenever
// storageAccountResourceId was supplied, which crash-looped every
// bring-your-own-storage deployment.
var effectiveStorageAccount = !empty(storageAccountResourceId)
  ? last(split(storageAccountResourceId, '/'))
  : (provisioningStorage ? storageAccountName : '')

// Composed rather than referenced: reading acr.properties.loginServer through a
// ternary makes ARM resolve the reference even when the branch is not taken.
// Public-cloud ACR login servers are deterministic.
var registryLoginServer = empty(registryName) ? '' : '${toLower(registryName)}.azurecr.io'

// ── Log Analytics + Application Insights ───────────────────────────────────

resource workspace 'Microsoft.OperationalInsights/workspaces@2022-10-01' = {
  name: workspaceName
  location: location
  tags: tags
  properties: {
    sku: { name: 'PerGB2018' }
    retentionInDays: 30
  }
}

// Workspace-based Application Insights. The app already emits OTLP traces and
// forwards panics/reported errors as exception events
// (internal/telemetry/errsink.go); before this, nothing consumed them.
resource appInsights 'Microsoft.Insights/components@2020-02-02' = {
  name: appInsightsName
  location: location
  tags: tags
  kind: 'web'
  properties: {
    Application_Type: 'web'
    WorkspaceResourceId: workspace.id
    IngestionMode: 'LogAnalytics'
  }
}

// ── Managed Environment ────────────────────────────────────────────────────
// Preview api-version is required for the managed OpenTelemetry agent
// (appInsightsConfiguration / openTelemetryConfiguration); the Container App
// itself stays on a GA version below.
//
// Only traces are routed. The app logs with slog to stdout, which
// appLogsConfiguration already ships to Log Analytics — it emits no OTLP logs,
// so an OTLP logs destination would be an empty pipe. App Insights does not
// accept OTLP metrics at all.
//
// The agent injects OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_PROTOCOL
// (always grpc) into the container. internal/telemetry/telemetry.go reads both,
// so tracing turns on with no further configuration.
resource env 'Microsoft.App/managedEnvironments@2024-10-02-preview' = {
  name: environmentName
  location: location
  tags: tags
  properties: {
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: workspace.properties.customerId
        sharedKey: workspace.listKeys().primarySharedKey
      }
    }
    appInsightsConfiguration: {
      connectionString: appInsights.properties.ConnectionString
    }
    openTelemetryConfiguration: {
      tracesConfiguration: {
        destinations: [ 'appInsights' ]
      }
    }
  }
}

// ── Managed identity (Blob data plane + ACR pull) ──────────────────────────

resource identity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: identityName
  location: location
  tags: tags
}

// ── Container registry (existing) + AcrPull ────────────────────────────────

resource acr 'Microsoft.ContainerRegistry/registries@2023-07-01' existing = if (!empty(registryName)) {
  name: registryName
}

@description('AcrPull (built-in role).')
var acrPullRoleId = '7f951dda-4ed3-4680-a7ca-43fe172d538d'

resource acrPullRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!empty(registryName)) {
  name: guid(identity.id, resourceId('Microsoft.ContainerRegistry/registries', registryName), acrPullRoleId)
  scope: acr
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', acrPullRoleId)
    principalId: identity.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

// ── Storage account (flow-content blob offload) ────────────────────────────

resource storage 'Microsoft.Storage/storageAccounts@2023-05-01' = if (provisioningStorage) {
  name: storageAccountName
  location: location
  tags: tags
  kind: 'StorageV2'
  sku: { name: 'Standard_LRS' }
  properties: {
    accessTier: 'Hot'
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: true
    allowBlobPublicAccess: false
    // Entra-only: the app authenticates with DefaultAzureCredential, so no
    // account key ever needs to exist in configuration.
    allowSharedKeyAccess: false
  }
}

// Data protection belongs on the blobServices/default CHILD, not on the
// storage account's own properties — the previous template put `blobProperties`
// under storageAccounts.properties, where no such property exists, so ARM
// dropped all of it and soft-delete, versioning and point-in-time restore were
// never actually enabled. The recovery procedure in README.md depends on them.
//
// changeFeed is mandatory for restorePolicy (point-in-time restore); it was
// missing before, which would have failed the deployment even if the block had
// been in the right place.
//
// There is deliberately NO lifecycle/management policy. Flow content is
// version-keyed (flows/{id}/content.v{N}.json), so the LIVE blob of a flow
// carries the mtime of its last edit: any age-based rule targets live data.
// The template used to tier to Archive at 90 days, which made every flow
// untouched for a quarter unreadable without rehydration. See README.md and
// internal/storage/database/postgres_flows.go.
resource blobService 'Microsoft.Storage/storageAccounts/blobServices@2023-05-01' = if (provisioningStorage) {
  name: 'default'
  parent: storage
  properties: {
    isVersioningEnabled: true
    changeFeed: {
      enabled: true
      retentionInDays: blobRetentionDays
    }
    deleteRetentionPolicy: {
      enabled: true
      days: blobRetentionDays
    }
    containerDeleteRetentionPolicy: {
      enabled: true
      days: blobRetentionDays
    }
    restorePolicy: {
      enabled: true
      days: blobRetentionDays - 1 // must be strictly less than the soft-delete window
    }
  }
}

resource flowContentContainer 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = if (provisioningStorage) {
  name: blobContainerName
  parent: blobService
  properties: {
    publicAccess: 'None'
  }
}

// Storage Blob Data CONTRIBUTOR. The previous template used
// '2a2b9908-6ea1-4ae2-b09c-2f025f3a3a9a', which is not a real role definition
// (Storage Blob Data Reader is 2a2b9908-6ea1-4ae2-8e65-a410df84e7d1) — the
// deployment would have failed with RoleDefinitionDoesNotExist, and had it
// resolved, read-only access would have failed every flow save.
@description('Storage Blob Data Contributor (built-in role).')
var storageBlobDataContributorRoleId = 'ba92f5b4-2d11-453d-a403-e96b0029c9fe'

// Scoped to the storage account, not the resource group, and only created when
// this template owns the account. A bring-your-own account may live in another
// resource group, which an RG-scoped deployment cannot target.
resource blobRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (provisioningStorage) {
  name: guid(identity.id, resourceId('Microsoft.Storage/storageAccounts', storageAccountName), storageBlobDataContributorRoleId)
  scope: storage
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', storageBlobDataContributorRoleId)
    principalId: identity.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

// ── Azure Cache for Redis (cross-replica backplane; off by default) ────────

resource redis 'Microsoft.Cache/redis@2023-08-01' = if (provisionRedis) {
  name: redisName
  location: location
  tags: tags
  properties: {
    sku: { name: redisSku, family: redisFamily, capacity: redisCapacity }
    enableNonSslPort: false
    minimumTlsVersion: '1.2'
    publicNetworkAccess: 'Enabled' // no VNet on this deployment; access is key-gated over TLS
    redisConfiguration: {
      'maxmemory-policy': 'allkeys-lru'
    }
  }
}

// ── Secrets and environment ────────────────────────────────────────────────
// Secrets live in Container App secrets, sourced from GitHub Actions secrets at
// deploy time. A Key Vault used to be provisioned here as well, but nothing
// ever wrote a secret into it: every boot did six lookups, got six 404s, logged
// six warnings, and fell back to exactly these container secrets.
//
// Empty entries are omitted rather than declared with an empty value: ACA
// rejects a secret with no value, and an env var pointing at a missing
// secretRef fails the revision.

var redisUrl = provisionRedis ? 'rediss://:${redis!.listKeys().primaryKey}@${redis!.properties.hostName}:6380' : ''

var secrets = concat(
  [
    { name: 'auth-secret', value: authSecret }
    { name: 'database-url', value: databaseUrl }
  ],
  empty(encryptionKey) ? [] : [ { name: 'encryption-key', value: encryptionKey } ],
  empty(encryptionKeyPrevious) ? [] : [ { name: 'encryption-key-previous', value: encryptionKeyPrevious } ],
  empty(metricsToken) ? [] : [ { name: 'metrics-token', value: metricsToken } ],
  empty(ciWebhookSecret) ? [] : [ { name: 'ci-webhook-secret', value: ciWebhookSecret } ],
  empty(smtpPassword) ? [] : [ { name: 'smtp-password', value: smtpPassword } ],
  empty(ssoClientSecret) ? [] : [ { name: 'sso-client-secret', value: ssoClientSecret } ],
  empty(notifyTeamsUrl) ? [] : [ { name: 'notify-teams-url', value: notifyTeamsUrl } ],
  empty(notifyTeamsWorkflowUrl) ? [] : [ { name: 'notify-teams-workflow-url', value: notifyTeamsWorkflowUrl } ],
  empty(notifyWebhookUrl) ? [] : [ { name: 'notify-webhook-url', value: notifyWebhookUrl } ],
  provisionRedis ? [ { name: 'redis-url', value: redisUrl } ] : []
)

var ingressFqdn = '${appName}.${env.properties.defaultDomain}'

// GOMEMLIMIT: ~80% of the replica's memory (the memory param, '<n>[.<d>]Gi'
// as ACA takes it), the rest left for non-heap memory (goroutine stacks, the
// runtime, the binary). Without it the GC paces by GOGC alone and ignores the
// container limit, so a burst of large uploads can grow the heap past it and
// get the replica OOM-killed. Keep it tied to `memory`, not a literal.
// last() rather than [1]: '1Gi' has no fractional part.
var memoryParts = split(replace(memory, 'Gi', ''), '.')
var memoryMiB = int(memoryParts[0]) * 1024 + (length(memoryParts) > 1 ? int(take(last(memoryParts), 1)) * 1024 / 10 : 0)
var goMemLimit = '${memoryMiB * 8 / 10}MiB'

// Per-replica pool (PAD_DB_MAX_OPEN_CONNS) from the server's connection cap, when dbServerMaxConnections is known:
// what the reserve leaves, shared by maxReplicas plus the rolling deploy's surge (in Single revision mode the new
// revision starts its minReplicas, at least one, beside the old revision's replicas until the swap; with the
// default minReplicas = 1 that is maxReplicas + 1), so every pool full at once still fits mid-deploy. At least 1:
// the app reads 0 as "unset" and opens its default. At most that default: a big server is no reason to open more
// than the app does without a cap (an explicit dbMaxOpenConns can, and wins).
// docs/architecture/operations.md has the arithmetic; internal/config/deploy_pool_test.go evaluates it.
var dbAppDefaultMaxOpenConns = 25 // storagedb.DefaultConfig; TestBicepPoolCeiling_IsTheAppDefault keeps them equal
var dbDerivedMaxOpenConns = min(dbAppDefaultMaxOpenConns, max(1, (dbServerMaxConnections - dbReservedConnections) / (maxReplicas + max(minReplicas, 1))))
var dbEffectiveMaxOpenConns = !empty(dbMaxOpenConns) ? dbMaxOpenConns : (dbServerMaxConnections > 0 ? string(dbDerivedMaxOpenConns) : '')

var coreEnv = [
  { name: 'PAD_MODE', value: 'cloud' }
  { name: 'PAD_HOST', value: '0.0.0.0' }
  { name: 'PAD_PORT', value: '8080' }
  { name: 'PAD_AUTH_ENABLED', value: 'true' }
  { name: 'PAD_STORAGE', value: 'database' }
  // How many replicas share the database: the boot check compares the pool with the server's max_connections.
  { name: 'PAD_MAX_REPLICAS', value: string(maxReplicas) }
  // ACA terminates TLS and presents a public HTTPS URL; the app runs HTTP
  // behind it.
  { name: 'PAD_BEHIND_PROXY', value: 'true' }
  { name: 'PAD_ALLOWED_ORIGINS', value: !empty(allowedOrigins) ? allowedOrigins : 'https://${ingressFqdn}' }
  { name: 'PAD_TRUSTED_PROXIES', value: trustedProxies }
  { name: 'PAD_APP_BASE_URL', value: !empty(appBaseUrl) ? appBaseUrl : 'https://${ingressFqdn}' }
  { name: 'PAD_FEATURE_DISABLE_SIGNUP', value: string(disableSignUp) }
  { name: 'PAD_AUDIT_SPILL_DIR', value: auditSpillDir }
  { name: 'GOMEMLIMIT', value: goMemLimit }
  // Tells DefaultAzureCredential which user-assigned identity to use, for both
  // blob and (if ever enabled) Postgres Entra auth.
  { name: 'AZURE_CLIENT_ID', value: identity.properties.clientId }
  { name: 'PAD_AUTH_SECRET', secretRef: 'auth-secret' }
  { name: 'PAD_DATABASE_URL', secretRef: 'database-url' }
]

var storageEnv = empty(effectiveStorageAccount) ? [] : [
  { name: 'PAD_AZURE_STORAGE_ACCOUNT', value: effectiveStorageAccount }
  { name: 'PAD_AZURE_STORAGE_CONTAINER', value: blobContainerName }
]

var optionalEnv = concat(
  empty(encryptionKey) ? [] : [ { name: 'PAD_ENCRYPTION_KEY', secretRef: 'encryption-key' } ],
  empty(encryptionKeyPrevious) ? [] : [ { name: 'PAD_ENCRYPTION_KEY_PREVIOUS', secretRef: 'encryption-key-previous' } ],
  empty(metricsToken) ? [] : [ { name: 'PAD_METRICS_TOKEN', secretRef: 'metrics-token' } ],
  empty(ciWebhookSecret) ? [] : [ { name: 'PAD_CI_WEBHOOK_SECRET', secretRef: 'ci-webhook-secret' } ],
  provisionRedis ? [ { name: 'PAD_REDIS_URL', secretRef: 'redis-url' } ] : [],
  empty(requestTimeout) ? [] : [ { name: 'PAD_REQUEST_TIMEOUT', value: requestTimeout } ],
  empty(logLevel) ? [] : [ { name: 'PAD_LOG_LEVEL', value: logLevel } ],
  empty(routePolicy) ? [] : [ { name: 'PAD_ROUTE_POLICY', value: routePolicy } ],
  empty(dbEffectiveMaxOpenConns) ? [] : [ { name: 'PAD_DB_MAX_OPEN_CONNS', value: dbEffectiveMaxOpenConns } ],
  dbServerMaxConnections > 0 ? [ { name: 'PAD_DB_SERVER_MAX_CONNS', value: string(dbServerMaxConnections) } ] : [],
  empty(auditRetentionDays) ? [] : [ { name: 'PAD_AUDIT_RETENTION_DAYS', value: auditRetentionDays } ],
  empty(scanInterval) ? [] : [ { name: 'PAD_SCAN_INTERVAL', value: scanInterval } ],
  empty(notifyTeamsUrl) ? [] : [ { name: 'PAD_NOTIFY_TEAMS_URL', secretRef: 'notify-teams-url' } ],
  empty(notifyTeamsWorkflowUrl) ? [] : [ { name: 'PAD_NOTIFY_TEAMS_WORKFLOW_URL', secretRef: 'notify-teams-workflow-url' } ],
  empty(notifyWebhookUrl) ? [] : [ { name: 'PAD_NOTIFY_WEBHOOK_URL', secretRef: 'notify-webhook-url' } ],
  empty(notifyAllowedHosts) ? [] : [ { name: 'PAD_NOTIFY_ALLOWED_HOSTS', value: notifyAllowedHosts } ],
  empty(azureFoundryAllowedHosts) ? [] : [ { name: 'PAD_AZURE_FOUNDRY_ALLOWED_HOSTS', value: azureFoundryAllowedHosts } ],
  aiAllowPrivateEgress ? [ { name: 'PAD_AI_ALLOW_PRIVATE_EGRESS', value: 'true' } ] : [],
  empty(embeddingProvider) ? [] : [ { name: 'PAD_EMBEDDING_PROVIDER', value: embeddingProvider } ],
  empty(embeddingDim) ? [] : [ { name: 'PAD_EMBEDDING_DIM', value: embeddingDim } ]
)

// Email is all-or-nothing: without it the app silently downgrades to a log-only
// mailer and password-reset links land in Log Analytics.
var emailEnv = (empty(smtpHost) || empty(emailFrom)) ? [] : concat(
  [
    { name: 'PAD_SMTP_HOST', value: smtpHost }
    { name: 'PAD_SMTP_PORT', value: smtpPort }
    { name: 'PAD_EMAIL_FROM', value: emailFrom }
  ],
  empty(smtpUsername) ? [] : [ { name: 'PAD_SMTP_USERNAME', value: smtpUsername } ],
  empty(smtpPassword) ? [] : [ { name: 'PAD_SMTP_PASSWORD', secretRef: 'smtp-password' } ]
)

// SSO is validated all-or-none at boot (internal/config/loader.go), so the
// template refuses to emit a partial set.
var ssoConfigured = !empty(ssoIssuer) && !empty(ssoClientId) && !empty(ssoRedirectUrl)

var ssoEnv = !ssoConfigured ? [] : concat(
  [
    { name: 'PAD_SSO_ISSUER', value: ssoIssuer }
    { name: 'PAD_SSO_CLIENT_ID', value: ssoClientId }
    { name: 'PAD_SSO_REDIRECT_URL', value: ssoRedirectUrl }
  ],
  empty(ssoProviderName) ? [] : [ { name: 'PAD_SSO_PROVIDER_NAME', value: ssoProviderName } ],
  empty(ssoClientSecret) ? [] : [ { name: 'PAD_SSO_CLIENT_SECRET', secretRef: 'sso-client-secret' } ]
)

// ── Container App ──────────────────────────────────────────────────────────

// The platform's SIGKILL deadline for a stopping replica; see terminationGracePeriodSeconds below.
var stopGraceSeconds = 35

resource containerApp 'Microsoft.App/containerApps@2023-05-01' = {
  name: appName
  location: location
  tags: tags
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
        allowInsecure: false
        traffic: [
          { weight: 100, latestRevision: true }
        ]
      }
      secrets: secrets
      // Managed-identity pull. RegistryCredentials.identity has existed since
      // api-version 2022-03-01, so this needs no version bump — and it removes
      // the long-lived registry PAT the GHCR path required at every restart and
      // scale event.
      registries: empty(registryName) ? [] : [
        {
          server: registryLoginServer
          identity: identity.id
        }
      ]
    }
    template: {
      // The app bounds its own stop with fx.StopTimeout; the steps and their
      // budgets are the table in docs/adr/0024-graceful-shutdown-and-server-restarting.md.
      // This is that bound plus a margin for the process to exit, so the
      // platform never cuts the stop short (internal/config/deploy_pool_test.go
      // keeps it above StopTimeout). The readiness alert below allows for the
      // probes a replica fails while it stops.
      terminationGracePeriodSeconds: stopGraceSeconds
      containers: [
        {
          name: appName
          image: image
          resources: {
            cpu: json(cpu)
            memory: memory
          }
          env: concat(coreEnv, storageEnv, optionalEnv, emailEnv, ssoEnv)
          volumeMounts: [
            // The key is volumeName, not name — `name` is silently ignored, so
            // the mount would not have resolved to the volume below.
            { volumeName: 'tmp', mountPath: '/tmp' }
          ]
          probes: [
            {
              // Liveness and readiness start only after this passes. /healthz answers once the
              // listener is up, which is after the boot-time schema migrations: without a startup
              // probe, a migration longer than the liveness budget (~100 s) got the container
              // killed mid-step and restarted into the same step.
              type: 'Startup'
              httpGet: { path: '/healthz', port: 8080 }
              initialDelaySeconds: probeInitialDelaySeconds
              periodSeconds: startupProbePeriodSeconds
              failureThreshold: startupFailureThreshold
            }
            {
              // /healthz is an unconditional 200 and deliberately does not
              // touch the database: a slow DB must not restart the container.
              type: 'Liveness'
              httpGet: { path: '/healthz', port: 8080 }
              initialDelaySeconds: probeInitialDelaySeconds
              periodSeconds: 30
              failureThreshold: 3
            }
            {
              // /readyz gates on Postgres, blob and (when configured) Redis.
              // A cold start runs schema migrations before it can answer, so
              // the failure threshold is generous.
              type: 'Readiness'
              httpGet: { path: '/readyz', port: 8080 }
              initialDelaySeconds: probeInitialDelaySeconds
              periodSeconds: probePeriodSeconds
              failureThreshold: readinessFailureThreshold
            }
          ]
        }
      ]
      volumes: [
        // ACA volume schema is { name, storageType }; `emptyDir: {}` is
        // Kubernetes syntax and is not a property here.
        { name: 'tmp', storageType: 'EmptyDir' }
      ]
      scale: {
        minReplicas: minReplicas
        maxReplicas: maxReplicas
      }
    }
  }
}

// ── Alerting ───────────────────────────────────────────────────────────────
// Every alert resource is conditional on alertEmails, so a deploy that does
// not pass it provisions NO alerting at all. The pipeline passes it. The list
// is in docs/architecture/operations.md.

var alertEmailList = !empty(alertEmails) ? split(alertEmails, ',') : []

resource actionGroup 'Microsoft.Insights/actionGroups@2023-01-01' = if (!empty(alertEmails)) {
  name: actionGroupName
  location: 'global'
  tags: tags
  properties: {
    groupShortName: take(replace(appName, '-', ''), 12)
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

// Container App restart count > 3 over 5 minutes: crash-loop, OOM-kill or
// failed readiness. Highest-signal single alert for a single-replica deploy.
resource restartAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = if (!empty(alertEmails)) {
  name: '${appName}-restarts'
  location: 'global'
  tags: tags
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
          name: 'restarts'
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
      { actionGroupId: actionGroup.id }
    ]
  }
}

// Log-query alerts on the workspace the environment already ships console and
// system logs to. The app exports ~40 Prometheus metrics that nothing scrapes
// yet, so until a scrape pipeline exists the structured JSON log IS the signal.
resource errorBurstAlert 'Microsoft.Insights/scheduledQueryRules@2023-03-15-preview' = if (!empty(alertEmails)) {
  name: '${appName}-error-burst'
  location: location
  tags: tags
  properties: {
    displayName: '${appName}: ERROR log burst'
    description: 'More than 20 ERROR-level log lines in 15 minutes. Filter ContainerAppConsoleLogs_CL on level ERROR; access-log lines carry request_id, and trace_id once the OpenTelemetry agent is forwarding to Application Insights.'
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

// Readiness failures one replica must log in the window before the alert
// fires. A replica stopping for a deploy answers /readyz with 503 from the
// start of its drain (ADR 0024) and then stops listening, so every probe until
// it exits fails: at most stopGraceSeconds / probePeriodSeconds + 1 of them.
// The alert needs one more than that, and at least readinessFailureThreshold,
// the count that takes a replica out of rotation. Counted per replica, so
// several replicas stopping together do not add up either.
var readinessAlertMinFailures = max(readinessFailureThreshold, stopGraceSeconds / probePeriodSeconds + 2)

resource readinessAlert 'Microsoft.Insights/scheduledQueryRules@2023-03-15-preview' = if (!empty(alertEmails)) {
  name: '${appName}-readiness-failing'
  location: location
  tags: tags
  properties: {
    displayName: '${appName}: readiness probe failing'
    description: 'A replica failed its readiness probe ${readinessAlertMinFailures} or more times in 10 minutes: enough to take it out of rotation, and more than a replica stopping for a deploy can log while it drains (503) and exits. With one replica the app is out of rotation: check /readyz (Postgres, blob, Redis) and the ERROR log.'
    severity: 1
    enabled: true
    evaluationFrequency: 'PT5M'
    windowSize: 'PT10M'
    scopes: [workspace.id]
    criteria: {
      allOf: [
        {
          query: 'ContainerAppSystemLogs_CL | where ContainerAppName_s == "${appName}" | where Log_s contains "readiness" and Log_s contains "fail" | summarize failures = count() by replica = tostring(column_ifexists("ReplicaName_s", "")) | where failures >= ${readinessAlertMinFailures}'
          timeAggregation: 'Count'
          operator: 'GreaterThan'
          threshold: 0
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

// Alerts on the fixed log messages of the background jobs, their locks and
// the shutdown (ADR 0023, ADR 0024; never reworded). Each matches the message
// as slog writes it, "msg":"<message>", so a longer message does not count.
// Normal operation has no alert: a job attempt skipped on a lock error (a
// job that keeps failing turns overdue, which does) and the drain of a stop.
var logMessageAlerts = [
  {
    name: 'job-overdue'
    displayName: 'background job overdue'
    description: 'A background job has had no successful run on any replica for longer than its threshold (twice its interval by default). The job field of the "job overdue" line and the pad_job_overdue gauge name it; its "job run failed" lines and its lock (a leaked lock shows here too) say why. Logged when it turns overdue and then hourly per replica.'
    severity: 2
    evaluationFrequency: 'PT15M'
    // Two hours, not one: the line repeats at most hourly per replica (at the
    // job's next attempt after the hour), so an hour-long window would let
    // the alert resolve and fire again every hour while the job stays overdue.
    windowSize: 'PT2H'
    messages: [
      'job overdue'
    ]
  }
  {
    name: 'job-lease-lost'
    displayName: 'background job lost its lock'
    description: 'A background job lost its cross-replica lock while it ran: its pinned database session died (failover, network cut, pg_terminate_backend) or its unlock found the lock gone, so another replica may have run the job beside it. Once after a database failover is expected; repeatedly, see the advisory-lock alert.'
    severity: 2
    evaluationFrequency: 'PT5M'
    windowSize: 'PT15M'
    messages: [
      'job lease lost'
    ]
  }
  {
    name: 'advisory-lock-lost'
    displayName: 'advisory lock not held by its session'
    description: 'An advisory lock was not on the session that took it: an unlock answered "not held", or a lease heartbeat found the lock gone. The usual cause is PgBouncer in transaction mode (Azure Flexible Server\'s built-in PgBouncer listens on 6432; the app warns at boot), which runs the lock and the unlock on different server sessions: connect to 5432 or pool in session mode. Once after a database failover or restart is expected.'
    severity: 2
    evaluationFrequency: 'PT5M'
    windowSize: 'PT15M'
    messages: [
      'advisory unlock reported not held; discarding connection'
      'advisory lease lost; discarding its session'
    ]
  }
  {
    name: 'chat-shutdown-workers'
    displayName: 'chat workers outlived the shutdown budget'
    description: 'At a stop, chat stream workers were still running when the chat shutdown budget ran out: their streams were ended with SERVER_RESTARTING in their place, and an answer that finished at that moment may not have been stored. Low impact; if it recurs, a provider call is not honouring cancellation.'
    severity: 3
    evaluationFrequency: 'PT1H'
    windowSize: 'P1D'
    messages: [
      'chat workers still running at shutdown'
    ]
  }
]

resource logMessageAlert 'Microsoft.Insights/scheduledQueryRules@2023-03-15-preview' = [for alert in logMessageAlerts: if (!empty(alertEmails)) {
  name: '${appName}-${alert.name}'
  location: location
  tags: tags
  properties: {
    displayName: '${appName}: ${alert.displayName}'
    description: alert.description
    severity: alert.severity
    enabled: true
    evaluationFrequency: alert.evaluationFrequency
    windowSize: alert.windowSize
    scopes: [workspace.id]
    criteria: {
      allOf: [
        {
          query: 'ContainerAppConsoleLogs_CL | where ContainerAppName_s == "${appName}" | where ${join(map(alert.messages, m => 'Log_s contains \'"msg":"${m}"\''), ' or ')}'
          timeAggregation: 'Count'
          operator: 'GreaterThan'
          threshold: 0
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
}]

output fqdn string = containerApp.properties.configuration.ingress.fqdn
output appUrl string = 'https://${containerApp.properties.configuration.ingress.fqdn}'
output containerAppName string = containerApp.name
output managedIdentityClientId string = identity.properties.clientId
output managedIdentityPrincipalId string = identity.properties.principalId
output storageAccountName string = effectiveStorageAccount
output blobContainerName string = blobContainerName
output redisName string = provisionRedis ? redisName : ''
output appInsightsName string = appInsights.name
output logAnalyticsWorkspaceId string = workspace.id
