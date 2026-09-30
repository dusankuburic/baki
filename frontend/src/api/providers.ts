import {request} from './client'
import type {
  ProviderInfo,
  ProviderTestResult,
  DeviceAuthResponse,
  GitHubAuthResult,
  GitHubUser,
  LocalModelsConfig,
  AzureScope,
  AzureConnection,
  AzureConnectionsListing,
  AzureConnectionCreateInput,
  AzureConnectionUpdateInput,
  AzureConnectionDeleteOptions,
  AzureConnectionDeleteResult,
  AzureModelEntry,
  AzureModelsListing,
  AzureModelCreateInput,
  AzureModelUpdateInput,
  AzureModelDeleteOptions,
  AzureDiscovery,
  AzureDiscoveryItem,
  AzureModelKind,
  AzureTestInput,
  AzureTestResult,
} from '@/types'

/**
 * The Azure AI Foundry registry route for a scope: the user's own under
 * /api/providers/azure-foundry, an organisation's under
 * /api/orgs/{orgId}/providers/azure-foundry. Each segment is URL-encoded.
 */
export function azurePath(scope: AzureScope, ...segments: string[]): string {
  const base =
    scope.kind === 'org'
      ? `/api/orgs/${encodeURIComponent(scope.orgId)}/providers/azure-foundry`
      : '/api/providers/azure-foundry'
  return segments.length > 0 ? `${base}/${segments.map(encodeURIComponent).join('/')}` : base
}

// `?cascade=true&force=true` for the flags that are set; '' for none.
function flagQuery(flags: Record<string, boolean | undefined>): string {
  const q = new URLSearchParams()
  for (const [name, on] of Object.entries(flags)) if (on) q.set(name, 'true')
  const s = q.toString()
  return s ? `?${s}` : ''
}

// The outbound routes (test, discover) wait on Azure — token, listing,
// probe — so they get more than the 30 s metadata default.
const AZURE_OUTBOUND_TIMEOUT_MS = 60_000

// A Go server serialises an empty (nil) slice as null: read it as empty
// rather than let `.map` on null crash a consumer.
function list<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : []
}

// The server leaves out an unknown model (omitempty); fill it and a missing
// kind in, so every item reads the same.
function discoveryItems(value: AzureDiscoveryItem[] | null | undefined, kind: AzureModelKind): AzureDiscoveryItem[] {
  return list(value).map(item =>
    item.kind && typeof item.model === 'string' ? item : {...item, kind: item.kind || kind, model: item.model ?? ''},
  )
}

export const providersApi = {
  // With an orgId the server also considers the organisation's shared
  // credentials; a provider that resolved to them carries `shared: true`.
  // `noDedup` bypasses the 5 s GET micro-cache for a refetch that must see a
  // change (a saved key, a deployment's served model revealed by a turn).
  listProviders: (orgId?: string, opts?: {noDedup?: boolean}): Promise<ProviderInfo[]> =>
    request(orgId ? `/api/providers/list?orgId=${encodeURIComponent(orgId)}` : '/api/providers/list', {
      method: 'GET',
      ...(opts?.noDedup ? {noDedup: true} : {}),
    }),

  saveApiKey: (provider: string, key: string): Promise<void> => request('/api/keys/save', {body: {provider, key}}),

  hasApiKey: (provider: string): Promise<boolean> => request('/api/keys/has', {body: {provider}}),

  deleteApiKey: (provider: string): Promise<void> => request('/api/keys/delete', {body: {provider}}),

  testProviderConnection: (provider: string): Promise<ProviderTestResult> =>
    request('/api/providers/test', {body: {provider}}),

  startGitHubAuth: (): Promise<DeviceAuthResponse> => request('/api/providers/github/start'),

  pollGitHubAuth: (deviceCode: string): Promise<GitHubAuthResult> =>
    request('/api/providers/github/poll', {body: {deviceCode}}),

  revokeGitHubAuth: (): Promise<void> => request('/api/providers/github/revoke'),

  getGitHubUser: (): Promise<GitHubUser | null> => request('/api/providers/github/user', {method: 'GET'}),

  startCopilotAuth: (): Promise<DeviceAuthResponse> => request('/api/providers/copilot/start'),

  pollCopilotAuth: (deviceCode: string): Promise<GitHubAuthResult> =>
    request('/api/providers/copilot/poll', {body: {deviceCode}}),

  revokeCopilotAuth: (): Promise<void> => request('/api/providers/copilot/revoke'),

  getCopilotUser: (): Promise<GitHubUser | null> => request('/api/providers/copilot/user', {method: 'GET'}),

  // The local OpenAI-compatible provider (Foundry Local / Ollama / LM Studio;
  // desktop only, no key). Removal and connection tests go through
  // deleteApiKey / testProviderConnection with provider 'local'. A 400 on
  // save or load arrives as an ApiError whose `code` is INVALID_BASE_URL (the
  // message says why) | INVALID_CONTEXT_WINDOW (save only: not a whole number
  // within 1024..2000000) | LOCAL_MODELS_UNAVAILABLE (a cloud server — the
  // feature is not offered; the GET answers it too). noDedup: the section
  // re-reads it right after a save or remove, and the 5 s GET micro-cache
  // would hand back the pre-change view.
  getLocalModelsConfig: (): Promise<LocalModelsConfig> =>
    request('/api/providers/local/config', {method: 'GET', noDedup: true}),

  // contextWindow is sent only when set; omitted, the server applies 8192.
  saveLocalModelsConfig: (baseUrl: string, contextWindow?: number): Promise<void> =>
    request('/api/providers/local/config', {
      body: contextWindow === undefined ? {baseUrl} : {baseUrl, contextWindow},
    }),

  // ---- Azure AI Foundry model registry (resources = "connections", and models) ----
  //
  // Every call takes the scope (azurePath). Reads need an org member, writes,
  // tests and discovery an org admin (403 otherwise); a deployment without
  // the encrypted org keystore answers ORG_KEYSTORE_UNAVAILABLE on every org
  // route. Failures arrive as ApiError with the server's `code` — 409s as
  // VersionConflictError, 403s as PermissionDeniedError:
  //   404 CONNECTION_NOT_FOUND | MODEL_NOT_FOUND
  //   409 CONNECTION_IN_USE {modelCount} | DUPLICATE_MODEL {existingModelId} |
  //       DUPLICATE_CONNECTION_NAME | TOO_MANY_CONNECTIONS {limit} |
  //       TOO_MANY_MODELS {limit} | CONFLICT (compare-and-swap) |
  //       REINDEX_REQUIRED (repeat with force after confirming a re-index)
  //   400 INVALID_KIND | INVALID_PURPOSE | INVALID_DESCRIPTION |
  //       INVALID_DISPLAY_NAME | INVALID_CONTEXT_WINDOW | INVALID_MAX_OUTPUT |
  //       INVALID_CONNECTION_NAME | INVALID_DESIGNATION |
  //       KIND_REQUIRES_PROJECT | DISCOVERY_UNAVAILABLE, plus the resource
  //       settings codes: INVALID_ENDPOINT, HOST_NOT_ALLOWED (the resource
  //       endpoint's host is not allow-listed), INVALID_PROJECT_ENDPOINT (its
  //       shape or host; the message says which), MISSING_SECRET (none stored,
  //       or this change may not carry it — the message says why),
  //       INVALID_AUTH, INVALID_DEPLOYMENT, INVALID_CONFIG, CONFIG_TOO_LARGE
  //   503 SECRET_STORAGE_UNAVAILABLE
  // The reads bypass the 5 s GET micro-cache: stores/azureFoundryStore dedupes
  // and caches them itself, and a reload right after a change must see it.

  // In user scope the listing also carries the shared resources of every
  // organisation where the caller is an admin or member (shared: true, with
  // orgId/orgName; read-only there). In org scope, only that org's.
  listAzureConnections: (scope: AzureScope): Promise<AzureConnectionsListing> =>
    request<Partial<AzureConnectionsListing> | null>(azurePath(scope, 'connections'), {
      method: 'GET',
      noDedup: true,
    }).then(res => ({
      connections: list(res?.connections),
      registryState: res?.registryState ?? 'ok',
      limits: res?.limits ?? {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: res?.knowledgeEmbeddingEnabled === true,
      ...(typeof res?.recoveredCopyPath === 'string' && res.recoveredCopyPath
        ? {recoveredCopyPath: res.recoveredCopyPath}
        : {}),
    })),

  // The contract fixes {connection} for POST and PUT only; the single GET is
  // read in either form.
  getAzureConnection: (scope: AzureScope, id: string): Promise<AzureConnection> =>
    request<{connection?: AzureConnection} | AzureConnection>(azurePath(scope, 'connections', id), {
      method: 'GET',
      noDedup: true,
    }).then(res => ('connection' in res && res.connection ? res.connection : (res as AzureConnection))),

  // 201 {connection}. TOO_MANY_CONNECTIONS at the limit; the settings codes
  // name the field at fault.
  createAzureConnection: (scope: AzureScope, input: AzureConnectionCreateInput): Promise<AzureConnection> =>
    request<{connection: AzureConnection}>(azurePath(scope, 'connections'), {body: input}).then(res => res.connection),

  // Also the way to dismiss discovery ({dismissDiscovery: true}). A stale
  // updatedAt / credentialUpdatedAt answers 409 CONFLICT; a new endpoint on
  // the resource that holds the knowledge-base model answers 409
  // REINDEX_REQUIRED unless `force` confirms the re-index.
  updateAzureConnection: (scope: AzureScope, id: string, input: AzureConnectionUpdateInput): Promise<AzureConnection> =>
    request<{connection: AzureConnection}>(azurePath(scope, 'connections', id), {method: 'PUT', body: input}).then(
      res => res.connection,
    ),

  // Deletes the stored credentials too. Without `cascade` a resource with
  // models answers 409 CONNECTION_IN_USE; `force` confirms the re-index when
  // it holds the knowledge-base model (409 REINDEX_REQUIRED otherwise).
  deleteAzureConnection: (
    scope: AzureScope,
    id: string,
    opts: AzureConnectionDeleteOptions = {},
  ): Promise<AzureConnectionDeleteResult> =>
    request(`${azurePath(scope, 'connections', id)}${flagQuery({cascade: opts.cascade, force: opts.force})}`, {
      method: 'DELETE',
    }),

  // The project's deployments and agents (Entra ID or the Azure CLI with a
  // project endpoint; DISCOVERY_UNAVAILABLE otherwise). An Azure refusal is a
  // 200 with `error`, and `alreadyAdded` marks what the registry holds.
  discoverAzureConnection: (scope: AzureScope, id: string): Promise<AzureDiscovery> =>
    request<Partial<AzureDiscovery> | null>(azurePath(scope, 'connections', id, 'discover'), {
      timeoutMs: AZURE_OUTBOUND_TIMEOUT_MS,
    }).then(res => ({
      deployments: discoveryItems(res?.deployments, 'chat'),
      agents: discoveryItems(res?.agents, 'agent'),
      warnings: list(res?.warnings),
      ...(res?.error ? {error: res.error} : {}),
    })),

  // Every chat, Claude, agent and embedding entry the scope sees (user scope:
  // the caller's own plus the shared entries of their organisations), and in
  // user scope the server's default for new chats ("m/<id>"), which AI
  // Accounts marks when the user saved no default of their own.
  listAzureModels: (scope: AzureScope): Promise<AzureModelsListing> =>
    request<{models?: AzureModelEntry[] | null; serverDefault?: string | null} | null>(azurePath(scope, 'models'), {
      method: 'GET',
      noDedup: true,
    }).then(res => ({
      models: list(res?.models),
      ...(typeof res?.serverDefault === 'string' && res.serverDefault ? {serverDefault: res.serverDefault} : {}),
    })),

  // 201 {model}. DUPLICATE_MODEL (same resource, kind namespace and name),
  // TOO_MANY_MODELS, KIND_REQUIRES_PROJECT (an agent on an API-key resource).
  createAzureModel: (scope: AzureScope, input: AzureModelCreateInput): Promise<AzureModelEntry> =>
    request<{model: AzureModelEntry}>(azurePath(scope, 'models'), {body: input}).then(res => res.model),

  // Also the designations: {isDefault: true} (org scope, "Default for
  // members") and {isKnowledgeEmbedding: true[, force: true]}.
  updateAzureModel: (scope: AzureScope, id: string, input: AzureModelUpdateInput): Promise<AzureModelEntry> =>
    request<{model: AzureModelEntry}>(azurePath(scope, 'models', id), {method: 'PUT', body: input}).then(
      res => res.model,
    ),

  // `force` confirms the re-index when it is the knowledge-base model of a
  // non-empty corpus (409 REINDEX_REQUIRED otherwise).
  deleteAzureModel: (scope: AzureScope, id: string, opts: AzureModelDeleteOptions = {}): Promise<{status: string}> =>
    request(`${azurePath(scope, 'models', id)}${flagQuery({force: opts.force})}`, {method: 'DELETE'}),

  // One route for a saved or draft resource and a saved or draft model (see
  // AzureTestInput). Any test outcome is a 200 {ok, latencyMs, error?, code?};
  // a 4xx means the request itself was invalid (a draft's settings code,
  // CONNECTION_NOT_FOUND, MODEL_NOT_FOUND).
  testAzure: (scope: AzureScope, input: AzureTestInput): Promise<AzureTestResult> =>
    request(azurePath(scope, 'test'), {body: input, timeoutMs: AZURE_OUTBOUND_TIMEOUT_MS}),
}
