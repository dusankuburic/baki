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

export function azurePath(scope: AzureScope, ...segments: string[]): string {
  const base =
    scope.kind === 'org'
      ? `/api/orgs/${encodeURIComponent(scope.orgId)}/providers/azure-foundry`
      : '/api/providers/azure-foundry'
  return segments.length > 0 ? `${base}/${segments.map(encodeURIComponent).join('/')}` : base
}

export function apiPath(path: string | undefined): string | undefined {
  return path && /^\/api(?:\/[A-Za-z0-9_.-]+)+$/.test(path) && !path.includes('..') ? path : undefined
}

function flagQuery(flags: Record<string, boolean | undefined>): string {
  const q = new URLSearchParams()
  for (const [name, on] of Object.entries(flags)) if (on) q.set(name, 'true')
  const s = q.toString()
  return s ? `?${s}` : ''
}

const AZURE_OUTBOUND_TIMEOUT_MS = 60_000

export const providersApi = {
  listProviders: async (orgId?: string, opts?: {noDedup?: boolean}): Promise<ProviderInfo[]> =>
    requestValidated(
      orgId ? `/api/providers/list?orgId=${encodeURIComponent(orgId)}` : '/api/providers/list',
      await getProviderListSchema(),
      {
        method: 'GET',
        ...(opts?.noDedup ? {noDedup: true} : {}),
      },
    ),

  getProviderCatalog: async (opts?: {noDedup?: boolean}): Promise<ProviderSpec[]> =>
    requestValidated('/api/providers/catalog', await getProviderCatalogSchema(), {
      method: 'GET',
      ...(opts?.noDedup ? {noDedup: true} : {}),
    }).then(c => c.providers),

  saveApiKey: (provider: string, key: string, extra?: Record<string, string>): Promise<void> =>
    request('/api/keys/save', {body: {...extra, provider, key}}),

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

  getGitHubUser: async (): Promise<GitHubUser | null> =>
    requestValidated('/api/providers/github/user', await getGitHubUserSchema(), {method: 'GET'}),

  startCopilotAuth: (): Promise<DeviceAuthResponse> => request('/api/providers/copilot/start'),

  pollCopilotAuth: (deviceCode: string): Promise<GitHubAuthResult> =>
    request('/api/providers/copilot/poll', {body: {deviceCode}}),

  revokeCopilotAuth: (): Promise<void> => request('/api/providers/copilot/revoke'),

  getCopilotUser: async (): Promise<GitHubUser | null> =>
    requestValidated('/api/providers/copilot/user', await getGitHubUserSchema(), {method: 'GET'}),

  startDeviceAuth: (path: string): Promise<DeviceAuthResponse> => request(path),

  pollDeviceAuth: (path: string, deviceCode: string): Promise<GitHubAuthResult> => request(path, {body: {deviceCode}}),

  revokeDeviceAuth: (path: string): Promise<void> => request(path),

  getDeviceAuthUser: async (path: string): Promise<GitHubUser | null> => {
    const u = await requestValidated(path, await getDeviceAuthUserSchema(), {method: 'GET'})
    if (!u) return null
    const login = u.login || u.name || u.email || (u.id != null ? String(u.id) : '')
    return {login, name: u.name || login}
  },

  getLocalModelsConfig: (): Promise<LocalModelsConfig> =>
    request('/api/providers/local/config', {method: 'GET', noDedup: true}),

  saveLocalModelsConfig: (baseUrl: string, contextWindow?: number): Promise<void> =>
    request('/api/providers/local/config', {
      body: contextWindow === undefined ? {baseUrl} : {baseUrl, contextWindow},
    }),

  listAzureConnections: async (scope: AzureScope): Promise<AzureConnectionsListing> =>
    requestValidated(azurePath(scope, 'connections'), await getAzureConnectionsListingSchema(), {
      method: 'GET',
      noDedup: true,
    }),

  getAzureConnection: async (scope: AzureScope, id: string): Promise<AzureConnection> =>
    requestValidated(azurePath(scope, 'connections', id), await getAzureConnectionResponseSchema(), {
      method: 'GET',
      noDedup: true,
    }),

  createAzureConnection: (scope: AzureScope, input: AzureConnectionCreateInput): Promise<AzureConnection> =>
    request<{connection: AzureConnection}>(azurePath(scope, 'connections'), {body: input}).then(res => res.connection),

  updateAzureConnection: (scope: AzureScope, id: string, input: AzureConnectionUpdateInput): Promise<AzureConnection> =>
    request<{connection: AzureConnection}>(azurePath(scope, 'connections', id), {method: 'PUT', body: input}).then(
      res => res.connection,
    ),

  deleteAzureConnection: (
    scope: AzureScope,
    id: string,
    opts: AzureConnectionDeleteOptions = {},
  ): Promise<AzureConnectionDeleteResult> =>
    request(`${azurePath(scope, 'connections', id)}${flagQuery({cascade: opts.cascade, force: opts.force})}`, {
      method: 'DELETE',
    }),

  discoverAzureConnection: async (scope: AzureScope, id: string): Promise<AzureDiscovery> =>
    requestValidated(azurePath(scope, 'connections', id, 'discover'), await getAzureDiscoverySchema(), {
      timeoutMs: AZURE_OUTBOUND_TIMEOUT_MS,
    }),

  listAzureModels: async (scope: AzureScope): Promise<AzureModelsListing> =>
    requestValidated(azurePath(scope, 'models'), await getAzureModelsListingSchema(), {
      method: 'GET',
      noDedup: true,
    }),

  createAzureModel: (scope: AzureScope, input: AzureModelCreateInput): Promise<AzureModelEntry> =>
    request<{model: AzureModelEntry}>(azurePath(scope, 'models'), {body: input}).then(res => res.model),

  updateAzureModel: (scope: AzureScope, id: string, input: AzureModelUpdateInput): Promise<AzureModelEntry> =>
    request<{model: AzureModelEntry}>(azurePath(scope, 'models', id), {method: 'PUT', body: input}).then(
      res => res.model,
    ),

  deleteAzureModel: (scope: AzureScope, id: string, opts: AzureModelDeleteOptions = {}): Promise<{status: string}> =>
    request(`${azurePath(scope, 'models', id)}${flagQuery({force: opts.force})}`, {method: 'DELETE'}),

  testAzure: (scope: AzureScope, input: AzureTestInput): Promise<AzureTestResult> =>
    request(azurePath(scope, 'test'), {body: input, timeoutMs: AZURE_OUTBOUND_TIMEOUT_MS}),
}
