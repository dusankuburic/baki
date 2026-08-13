import {request} from './client'
import type {ProviderInfo, ProviderTestResult, DeviceAuthResponse, GitHubAuthResult, GitHubUser} from '@/types'

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
}
