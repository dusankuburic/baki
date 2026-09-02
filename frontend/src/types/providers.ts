// AI provider catalogue + auth flows (OAuth device-code, GitHub Copilot/Models).

export type ProviderID = 'claude' | 'openai' | 'gemini' | 'xai' | 'glm' | 'github-models' | 'copilot' | 'demo'

export type AuthType = 'api_key' | 'oauth'

export interface ModelDetail {
  id: string
  displayName: string
  contextLimit: number
  inputCostPerM: number
  outputCostPerM: number
  /** Provider-family tool support (native or marker-based fallback). */
  supportsTools?: boolean
}

export interface ProviderInfo {
  id: ProviderID
  name: string
  authType: AuthType
  contextLimit: number
  models: ModelDetail[]
  defaultModel: string
  configured: boolean
}

export interface ProviderTestResult {
  ok: boolean
  latencyMs: number
  error?: string
}

export interface SourceFileInfo {
  filename: string
  subflowId: string
  subflowName: string
  blockCount: number
  lineCount: number
}

export interface DeviceAuthResponse {
  device_code: string
  user_code: string
  verification_uri: string
  expires_in: number
  interval: number
}

export interface GitHubAuthResult {
  status: string
  token?: string
  error?: string
}

export interface GitHubUser {
  login: string
  name: string
}
