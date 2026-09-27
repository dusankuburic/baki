import {request} from './client'
import type {
  ProviderInfo,
  ProviderTestResult,
  DeviceAuthResponse,
  GitHubAuthResult,
  GitHubUser,
  AzureFoundryPublicConfig,
  AzureFoundryConfigInput,
} from '@/types'

export const providersApi = {
  listProviders: (): Promise<ProviderInfo[]> => request('/api/providers/list', {method: 'GET'}),

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
  // one of INVALID_ENDPOINT | HOST_NOT_ALLOWED | MISSING_SECRET |
  // CONFIG_TOO_LARGE | INVALID_DEPLOYMENT | INVALID_AUTH.
  saveAzureFoundryConfig: (cfg: AzureFoundryConfigInput): Promise<void> =>
    request('/api/providers/azure-foundry/config', {body: cfg}),
}
