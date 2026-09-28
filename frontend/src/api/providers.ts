import {request} from './client'
import type {
  ProviderInfo,
  ProviderTestResult,
  DeviceAuthResponse,
  GitHubAuthResult,
  GitHubUser,
  AzureFoundryPublicConfig,
  AzureFoundryConfigInput,
  OrgAzureFoundryConfigInput,
} from '@/types'

// The org-scoped Azure AI Foundry routes (cloud only). Any member may read
// the redacted view; saving, removing and testing need an org admin (403
// otherwise). A member's own credentials always win over the org's.
function orgAzurePath(orgId: string, leaf: 'config' | 'test'): string {
  return `/api/orgs/${encodeURIComponent(orgId)}/providers/azure-foundry/${leaf}`
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

  // noDedup: the section re-reads this right after a save or remove, and the
  // 5 s GET micro-cache would hand back the pre-change view.
  getAzureFoundryConfig: (): Promise<AzureFoundryPublicConfig> =>
    request('/api/providers/azure-foundry/config', {method: 'GET', noDedup: true}),

  // Removal and connection tests go through deleteApiKey / testProviderConnection
  // with provider 'azure-foundry'. A 400 arrives as an ApiError whose `code` is
  // one of INVALID_ENDPOINT | HOST_NOT_ALLOWED (the resource endpoint's host is
  // not allow-listed) | INVALID_PROJECT_ENDPOINT (the project endpoint's shape
  // or host; the message says which) | MISSING_SECRET (no secret stored, the
  // auth mode changed, or the endpoint changed — a stored secret is never
  // carried to another resource; the message says which) | CONFIG_TOO_LARGE |
  // INVALID_DEPLOYMENT (also: more than 16 additional or 20 Claude
  // deployments) | INVALID_AUTH | INVALID_CONFIG (an unknown or misspelled
  // field, named in the message) | MISSING_DEPLOYMENT (no chat target named:
  // chatDeployment, deployments and claudeDeployments are all empty — an API
  // key cannot list the resource's deployments; only Entra ID with a project
  // endpoint can). Every code carries a user-readable message. The chat model
  // picker lists exactly the named deployments (chatDeployment + deployments +
  // claudeDeployments as "claude:<name>"), plus the project's deployments and
  // agents with Entra ID + project.
  saveAzureFoundryConfig: (cfg: AzureFoundryConfigInput): Promise<void> =>
    request('/api/providers/azure-foundry/config', {body: cfg}),

  // Org-scoped variants of the four calls above. Same public shape (plus
  // `shared`, `updatedBy`, `updatedAt`), same body and error codes on save;
  // noDedup for the same post-save reload reason. Beyond those: the GET
  // answers 500 ORG_CREDENTIAL_UNREADABLE when the stored credential cannot
  // be decrypted or read (never a silent "not configured"), every route
  // answers 404 ORG_KEYSTORE_UNAVAILABLE on a deployment without org
  // credentials and 403 to a caller without the right role, and the save
  // takes the `updatedAt` last read as a compare-and-swap guard — 409
  // CONFLICT when another admin saved in between.
  getOrgAzureFoundryConfig: (orgId: string): Promise<AzureFoundryPublicConfig> =>
    request(orgAzurePath(orgId, 'config'), {method: 'GET', noDedup: true}),

  saveOrgAzureFoundryConfig: (orgId: string, cfg: OrgAzureFoundryConfigInput): Promise<void> =>
    request(orgAzurePath(orgId, 'config'), {body: cfg}),

  deleteOrgAzureFoundryConfig: (orgId: string): Promise<void> =>
    request(orgAzurePath(orgId, 'config'), {method: 'DELETE'}),

  testOrgAzureFoundryConnection: (orgId: string): Promise<ProviderTestResult> => request(orgAzurePath(orgId, 'test')),
}
