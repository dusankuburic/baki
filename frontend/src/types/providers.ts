// AI provider catalogue + auth flows (OAuth device-code, GitHub Copilot/Models).

export type ProviderID = 'claude' | 'openai' | 'gemini' | 'xai' | 'glm' | 'github-models' | 'copilot' | 'azure-foundry'

// PROVIDERS is the client-side catalogue of every ProviderID. `satisfies`
// turns a new ID in the union into a compile error here until it has an
// entry, so pickers derived from it cannot silently drop a provider — the
// active-assistant picker once hardcoded four of seven, and a settings file
// naming one of the missing three rendered no selected segment.
export const PROVIDERS = {
  claude: {label: 'Claude'},
  openai: {label: 'OpenAI'},
  gemini: {label: 'Gemini'},
  xai: {label: 'xAI'},
  glm: {label: 'GLM'},
  'github-models': {label: 'GitHub Models'},
  copilot: {label: 'Copilot'},
  'azure-foundry': {label: 'Azure AI Foundry'},
} as const satisfies Record<ProviderID, {label: string}>

export const PROVIDER_OPTIONS: {value: ProviderID; label: string}[] = (Object.keys(PROVIDERS) as ProviderID[]).map(
  id => ({value: id, label: PROVIDERS[id].label}),
)

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
  /**
   * True when the credentials came from the active organisation rather than
   * the member (GET /api/providers/list?orgId=…). A member's own credentials
   * always win; the org's are the fallback.
   */
  shared?: boolean
}

export interface ProviderTestResult {
  ok: boolean
  latencyMs: number
  error?: string
}

// Azure AI Foundry is configured with a document (endpoint + credentials +
// optional deployments) rather than a single key, via
// /api/providers/azure-foundry/config. Removal and connection tests reuse the
// generic key endpoints with provider id 'azure-foundry'.

export type AzureFoundryAuthMode = 'api_key' | 'entra'

/**
 * GET /api/providers/azure-foundry/config — the redacted view. Secrets never
 * come back; `hasSecret` only says whether one is stored, so an edit that
 * leaves the secret blank keeps it.
 */
export type AzureFoundryPublicConfig =
  | {configured: false}
  | {
      configured: true
      endpoint: string
      authMode: AzureFoundryAuthMode
      hasSecret: boolean
      tenantId?: string
      clientId?: string
      projectEndpoint?: string
      chatDeployment?: string
      embeddingDeployment?: string
      /** Additional chat deployments offered in the model picker (normalised by the server). */
      deployments?: string[]
      /** Claude deployments (Anthropic Messages route); listed as "claude:<name>". */
      claudeDeployments?: string[]
      /**
       * Org-scoped view (GET /api/orgs/{orgId}/providers/azure-foundry/config)
       * only: the credentials are the organisation's, and `updatedBy` /
       * `updatedAt` say which admin saved them and when (RFC 3339).
       */
      shared?: boolean
      updatedBy?: string
      updatedAt?: string
    }

/** Credential half of the POST body. A missing secret keeps the stored one. */
export type AzureFoundryAuthInput =
  {mode: 'api_key'; key?: string} | {mode: 'entra'; tenantId: string; clientId: string; clientSecret?: string}

/**
 * POST /api/providers/azure-foundry/config body. An API key cannot list the
 * resource's deployments, so the chat model picker shows exactly
 * `chatDeployment` plus `deployments` (at most 16); the server requires one of
 * them unless Entra ID with a project endpoint lists the project's deployments.
 */
export interface AzureFoundryConfigInput {
  endpoint: string
  auth: AzureFoundryAuthInput
  projectEndpoint?: string
  chatDeployment?: string
  embeddingDeployment?: string
  /** Additional chat deployments; omitted when empty. */
  deployments?: string[]
  /** Claude deployments; omitted when empty. */
  claudeDeployments?: string[]
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
