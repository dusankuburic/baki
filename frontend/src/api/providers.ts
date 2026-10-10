import {request, requestValidated} from './client'
import {
  getAzureConnectionsListingSchema,
  getAzureConnectionResponseSchema,
  getAzureDiscoverySchema,
  getAzureModelsListingSchema,
  getDeviceAuthUserSchema,
  getGitHubUserSchema,
  getProviderCatalogSchema,
  getProviderListSchema,
} from './schemas'
import type {
  ProviderInfo,
  ProviderSpec,
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

/**
 * A route the catalogue names, if it is a path under /api/ (segments of
 * letters, digits, `-`, `_` and `.`; no query, fragment or empty segment).
 * Anything else — an absolute URL, a traversal — reads as "no route", and
 * the section renders the generic card instead of calling it.
 */
export function apiPath(path: string | undefined): string | undefined {
  return path && /^\/api(?:\/[A-Za-z0-9_.-]+)+$/.test(path) && !path.includes('..') ? path : undefined
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

export const providersApi = {
  // With an orgId the server also considers the organisation's shared
  // credentials; a provider that resolved to them carries `shared: true`.
  // `noDedup` bypasses the 5 s GET micro-cache for a refetch that must see a
  // change (a saved key, a deployment's served model revealed by a turn).
  listProviders: async (orgId?: string, opts?: {noDedup?: boolean}): Promise<ProviderInfo[]> =>
    requestValidated(
      orgId ? `/api/providers/list?orgId=${encodeURIComponent(orgId)}` : '/api/providers/list',
      await getProviderListSchema(),
      {
        method: 'GET',
        ...(opts?.noDedup ? {noDedup: true} : {}),
      },
    ),

  // GET /api/providers/catalog (Phase 4b): every provider the server
  // registers for its profile, configured or not, in display order, with its
  // credential form (types/providers ProviderSpec). A server older than this
  // bundle has no such route: the 404 arrives as an ApiError and the caller
  // renders its built-in sections. `noDedup` as for listProviders.
  getProviderCatalog: async (opts?: {noDedup?: boolean}): Promise<ProviderSpec[]> =>
    requestValidated('/api/providers/catalog', await getProviderCatalogSchema(), {
      method: 'GET',
      ...(opts?.noDedup ? {noDedup: true} : {}),
    }).then(c => c.providers),

  // `extra` carries an api_key schema's other fields by name; the body is
  // {provider, key} alone for the usual one-field form.
  saveApiKey: (provider: string, key: string, extra?: Record<string, string>): Promise<void> =>
    request('/api/keys/save', {body: {...extra, provider, key}}),

  // {has: boolean} on the wire; a bare boolean is read too, in case a server
  // ever answered one. Anything else is "no key".
  hasApiKey: (provider: string): Promise<boolean> =>
    request<{has?: unknown} | boolean | null>('/api/keys/has', {body: {provider}}).then(r =>
      typeof r === 'boolean' ? r : r?.has === true,
    ),

  deleteApiKey: (provider: string): Promise<void> => request('/api/keys/delete', {body: {provider}}),

  testProviderConnection: (provider: string): Promise<ProviderTestResult> =>
    request('/api/providers/test', {body: {provider}}),

  startGitHubAuth: (): Promise<DeviceAuthResponse> => request('/api/providers/github/start'),

  pollGitHubAuth: (deviceCode: string): Promise<GitHubAuthResult> =>
    request('/api/providers/github/poll', {body: {deviceCode}}),

  revokeGitHubAuth: (): Promise<void> => request('/api/providers/github/revoke'),

  // null (not connected — no stored token) is a valid answer, not a failure.
  getGitHubUser: async (): Promise<GitHubUser | null> =>
    requestValidated('/api/providers/github/user', await getGitHubUserSchema(), {method: 'GET'}),

  startCopilotAuth: (): Promise<DeviceAuthResponse> => request('/api/providers/copilot/start'),

  pollCopilotAuth: (deviceCode: string): Promise<GitHubAuthResult> =>
    request('/api/providers/copilot/poll', {body: {deviceCode}}),

  revokeCopilotAuth: (): Promise<void> => request('/api/providers/copilot/revoke'),

  // See getGitHubUser: null means not connected.
  getCopilotUser: async (): Promise<GitHubUser | null> =>
    requestValidated('/api/providers/copilot/user', await getGitHubUserSchema(), {method: 'GET'}),

  // The device-code flow at the routes a catalogue entry names
  // (ProviderSpec.endpoints: start, poll, revoke, user), for a provider the
  // bundle has no bespoke pair for. Each takes the path as the server sent
  // it; the caller has checked it is a path under /api/ (apiPath).
  startDeviceAuth: (path: string): Promise<DeviceAuthResponse> => request(path),

  pollDeviceAuth: (path: string, deviceCode: string): Promise<GitHubAuthResult> => request(path, {body: {deviceCode}}),

  revokeDeviceAuth: (path: string): Promise<void> => request(path),

  // See getGitHubUser: null means not connected. The shape is the
  // provider's own: its login, else its name, e-mail or id, names the user.
  getDeviceAuthUser: async (path: string): Promise<GitHubUser | null> => {
    const u = await requestValidated(path, await getDeviceAuthUserSchema(), {method: 'GET'})
    if (!u) return null
    const login = u.login || u.name || u.email || (u.id != null ? String(u.id) : '')
    return {login, name: u.name || login}
  },

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
  // The schema reads a nil connections slice as [] (Go null on the wire) and
  // requires the registryState/limits flags the listing struct always sends.
  listAzureConnections: async (scope: AzureScope): Promise<AzureConnectionsListing> =>
    requestValidated(azurePath(scope, 'connections'), await getAzureConnectionsListingSchema(), {
      method: 'GET',
      noDedup: true,
    }),

  // The contract fixes {connection} for POST and PUT only; the single GET is
  // read in either form ({connection} envelope or the bare resource).
  getAzureConnection: async (scope: AzureScope, id: string): Promise<AzureConnection> =>
    requestValidated(azurePath(scope, 'connections', id), await getAzureConnectionResponseSchema(), {
      method: 'GET',
      noDedup: true,
    }),

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
  discoverAzureConnection: async (scope: AzureScope, id: string): Promise<AzureDiscovery> =>
    requestValidated(azurePath(scope, 'connections', id, 'discover'), await getAzureDiscoverySchema(), {
      timeoutMs: AZURE_OUTBOUND_TIMEOUT_MS,
    }),

  // Every chat, Claude, agent and embedding entry the scope sees (user scope:
  // the caller's own plus the shared entries of their organisations), and in
  // user scope the server's default for new chats ("m/<id>"), which AI
  // Accounts marks when the user saved no default of their own.
  listAzureModels: async (scope: AzureScope): Promise<AzureModelsListing> =>
    requestValidated(azurePath(scope, 'models'), await getAzureModelsListingSchema(), {
      method: 'GET',
      noDedup: true,
    }),

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
