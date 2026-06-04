import {request} from './client'
import type {ProviderInfo, ProviderTestResult, DeviceAuthResponse, GitHubAuthResult, GitHubUser} from '@/types/domain'

export const providersApi = {
  listProviders: (): Promise<ProviderInfo[]> =>
    request('/api/providers/list', undefined, 'GET'),

  saveApiKey: (provider: string, key: string): Promise<void> =>
    request('/api/keys/save', { provider, key }),

  hasApiKey: (provider: string): Promise<boolean> =>
    request('/api/keys/has', { provider }),

  deleteApiKey: (provider: string): Promise<void> =>
    request('/api/keys/delete', { provider }),

  testProviderConnection: (provider: string): Promise<ProviderTestResult> =>
    request('/api/providers/test', { provider }),

  startGitHubAuth: (): Promise<DeviceAuthResponse> =>
    request('/api/providers/github/start'),

  pollGitHubAuth: (deviceCode: string): Promise<GitHubAuthResult> =>
    request('/api/providers/github/poll', { deviceCode }),

  revokeGitHubAuth: (): Promise<void> =>
    request('/api/providers/github/revoke'),

  getGitHubUser: (): Promise<GitHubUser | null> =>
    request('/api/providers/github/user', undefined, 'GET'),

  startCopilotAuth: (): Promise<DeviceAuthResponse> =>
    request('/api/providers/copilot/start'),

  pollCopilotAuth: (deviceCode: string): Promise<GitHubAuthResult> =>
    request('/api/providers/copilot/poll', { deviceCode }),

  revokeCopilotAuth: (): Promise<void> =>
    request('/api/providers/copilot/revoke'),

  getCopilotUser: (): Promise<GitHubUser | null> =>
    request('/api/providers/copilot/user', undefined, 'GET'),
}
