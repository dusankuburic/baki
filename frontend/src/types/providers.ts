
import type {Tolerant} from './tolerant'

export type KnownProviderID =
  'claude' | 'openai' | 'gemini' | 'xai' | 'glm' | 'github-models' | 'copilot' | 'azure-foundry' | 'local'

export type ProviderID = string

export const PROVIDERS = {
  claude: {label: 'Claude'},
  openai: {label: 'OpenAI'},
  gemini: {label: 'Gemini'},
  xai: {label: 'xAI'},
  glm: {label: 'GLM'},
  'github-models': {label: 'GitHub Models'},
  copilot: {label: 'Copilot'},
  'azure-foundry': {label: 'Azure AI Foundry'},
  local: {label: 'Local model'},
} as const satisfies Record<KnownProviderID, {label: string}>

export const PROVIDER_IDS: readonly KnownProviderID[] = Object.keys(PROVIDERS) as KnownProviderID[]

export const PROVIDER_OPTIONS: {value: KnownProviderID; label: string}[] = PROVIDER_IDS.map(id => ({
  value: id,
  label: PROVIDERS[id].label,
}))

export function isKnownProviderId(value: unknown): value is KnownProviderID {
  return typeof value === 'string' && Object.prototype.hasOwnProperty.call(PROVIDERS, value)
}

export const AUTH_TYPES = ['api_key', 'oauth'] as const
export type AuthType = (typeof AUTH_TYPES)[number]

export const CREDENTIAL_KINDS = ['api_key', 'oauth_device', 'document', 'none'] as const
export type CredentialKind = (typeof CREDENTIAL_KINDS)[number]

export const CREDENTIAL_FIELD_TYPES = ['text', 'secret', 'url', 'select', 'number', 'json'] as const
export type CredentialFieldType = (typeof CREDENTIAL_FIELD_TYPES)[number]

export const PROVIDER_MODES = ['cloud', 'desktop'] as const
export type ProviderMode = (typeof PROVIDER_MODES)[number]

export const PROVIDER_PRICINGS = ['catalog', 'free', 'unpriced'] as const
export type ProviderPricing = (typeof PROVIDER_PRICINGS)[number]

export const PROVIDER_CAP_EMBEDDINGS = 'embeddings'
/** Model ids are deployment aliases (Azure AI Foundry): the knowledge base embeds with such a provider only on the
 * operator's PAD_EMBEDDING_PROVIDER pin. Mirrors internal/ai CapModelAliases. */
export const PROVIDER_CAP_MODEL_ALIASES = 'model_aliases'

export interface CredentialFieldOption {
  value: string
  /** What a person reads; the client shows the value where it is missing or empty (older servers sent the value
   * itself here). */
  label?: string
}

export interface CredentialField {
  name: string
  label: string
  type: Tolerant<CredentialFieldType>
  required: boolean
  placeholder?: string
  help?: string
  options?: CredentialFieldOption[]
}

export interface CredentialSchema {
  kind: Tolerant<CredentialKind>
  fields: CredentialField[]
  helpUrl?: string
}

export interface ProviderEndpoints {
  save?: string
  test?: string
  start?: string
  poll?: string
  revoke?: string
  user?: string
}

export interface ProviderSpec {
  id: ProviderID
  name: string
  authType: Tolerant<AuthType>
  modes: Tolerant<ProviderMode>[]
  caps: string[]
  credential: CredentialSchema
  endpoints: ProviderEndpoints
  configured: boolean
  color?: string
  icon?: string
  pricing?: Tolerant<ProviderPricing>
}

export interface ProviderCatalog {
  providers: ProviderSpec[]
}

export interface ModelDetail {
  id: string
  displayName: string
  contextLimit: number
  inputCostPerM: number
  outputCostPerM: number
  supportsTools?: boolean
  kind?: Tolerant<AzureModelKind>
  purpose?: Tolerant<ModelPurpose>
  purposeCustom?: string
  description?: string
  shared?: boolean
  connectionId?: string
  connectionName?: string
  status?: Tolerant<AzureStatus>
  statusReason?: string
  aliases?: string[]
  isDefault?: boolean
  isOrgDefault?: boolean
}

export interface ProviderInfo {
  id: ProviderID
  name: string
  authType: Tolerant<AuthType>
  contextLimit: number
  models: ModelDetail[]
  defaultModel: string
  configured: boolean
  shared?: boolean
  suggestDiscovery?: boolean
  modes?: Tolerant<ProviderMode>[]
  caps?: string[]
  credential?: CredentialSchema
  endpoints?: ProviderEndpoints
  color?: string
  icon?: string
  pricing?: Tolerant<ProviderPricing>
}

export interface ProviderTestResult {
  ok: boolean
  latencyMs: number
  error?: string
  code?: string
}

export const AZURE_AUTH_MODES = ['api_key', 'entra', 'entra_cli'] as const
export type AzureFoundryAuthMode = (typeof AZURE_AUTH_MODES)[number]

export type AzureFoundryAuthInput =
  | {mode: 'api_key'; key?: string}
  | {mode: 'entra'; tenantId: string; clientId: string; clientSecret?: string}
  | {mode: 'entra_cli'; tenantId?: string}

export type AzureScope = {kind: 'user'} | {kind: 'org'; orgId: string}

export const AZURE_MODEL_KINDS = ['chat', 'claude', 'agent', 'embedding'] as const
export type AzureModelKind = (typeof AZURE_MODEL_KINDS)[number]
export const CHAT_MODEL_KINDS = ['chat', 'claude', 'agent'] as const satisfies readonly AzureModelKind[]

export type ModelPurposePreset = 'general' | 'documentation' | 'fixes' | 'review' | 'refactoring' | 'testing'
export type ModelPurpose = ModelPurposePreset | 'custom'

export const AZURE_STATUSES = ['ready', 'needs_credentials', 'unreadable', 'invalid_config', 'unsupported'] as const
export type AzureStatus = (typeof AZURE_STATUSES)[number]

export const AZURE_REGISTRY_STATES = ['ok', 'read_only', 'recovered'] as const
export type AzureRegistryState = (typeof AZURE_REGISTRY_STATES)[number]

export interface AzureRegistryLimits {
  connections: number
  models: number
}

export interface AzureLastTest {
  ok: boolean
  latencyMs?: number
  error?: string
  at: string
}

export interface AzureConnectionPublicConfig {
  endpoint: string
  authMode: Tolerant<AzureFoundryAuthMode> | ''
  hasSecret: boolean
  tenantId?: string
  clientId?: string
  projectEndpoint?: string
}

export interface AzureConnection {
  id: string
  name: string
  shared: boolean
  orgId?: string
  orgName?: string
  legacy: boolean
  config: AzureConnectionPublicConfig
  status: Tolerant<AzureStatus>
  statusReason?: string
  canDiscover: boolean
  supportsAgents: boolean
  suggestDiscovery: boolean
  modelCount: number
  lastTest?: AzureLastTest | null
  updatedAt: string
  credentialUpdatedAt?: string
  sharedBy?: string
}

export interface AzureModelEntry {
  id: string
  modelId: string
  connectionId: string
  connectionName: string
  kind: Tolerant<AzureModelKind>
  deployment: string
  target: string
  displayName: string
  purpose: Tolerant<ModelPurpose>
  purposeCustom?: string
  description?: string
  contextWindow?: number
  reportedContextWindow?: number
  maxOutputTokens?: number
  isDefault: boolean
  isKnowledgeEmbedding: boolean
  shared: boolean
  orgId?: string
  orgName?: string
  status: Tolerant<AzureStatus>
  statusReason?: string
  lastTest?: AzureLastTest | null
  updatedAt: string
}

export interface AzureConnectionsListing {
  connections: AzureConnection[]
  registryState: Tolerant<AzureRegistryState>
  limits: AzureRegistryLimits
  knowledgeEmbeddingEnabled: boolean
  recoveredCopyPath?: string
}

export interface AzureModelsListing {
  models: AzureModelEntry[]
  serverDefault?: string
}

export interface AzureDiscoveryItem {
  name: string
  kind: AzureModelKind
  model: string
  contextWindow?: number
  alreadyAdded: boolean
  modelId?: string
  deployment?: string
  workflow?: boolean
}

export interface AzureDiscovery {
  deployments: AzureDiscoveryItem[]
  agents: AzureDiscoveryItem[]
  warnings: string[]
  error?: string
}

export interface AzureConnectionConfigInput {
  endpoint: string
  auth: AzureFoundryAuthInput
  projectEndpoint?: string
}

export interface AzureConnectionCreateInput {
  name: string
  config: AzureConnectionConfigInput
}

export interface AzureConnectionUpdateInput {
  name?: string
  config?: AzureConnectionConfigInput
  updatedAt?: string
  credentialUpdatedAt?: string
  dismissDiscovery?: boolean
  force?: boolean
}

export interface AzureConnectionDeleteOptions {
  cascade?: boolean
  force?: boolean
}

export interface AzureConnectionDeleteResult {
  status: string
  deletedModels: number
  clearedKnowledgeEmbedding: boolean
}

export interface AzureModelCreateInput {
  connectionId: string
  kind: AzureModelKind
  deployment: string
  displayName?: string
  purpose?: ModelPurpose
  purposeCustom?: string
  description?: string
  contextWindow?: number
  maxOutputTokens?: number
  isDefault?: boolean
  isKnowledgeEmbedding?: boolean
  force?: boolean
}

export interface AzureModelUpdateInput {
  connectionId?: string
  kind?: AzureModelKind
  deployment?: string
  displayName?: string
  purpose?: ModelPurpose
  purposeCustom?: string
  description?: string
  contextWindow?: number | null
  maxOutputTokens?: number | null
  isDefault?: boolean
  isKnowledgeEmbedding?: boolean
  updatedAt?: string
  force?: boolean
}

export interface AzureModelDeleteOptions {
  force?: boolean
}

export type AzureTestModelRef = {id: string} | {kind: AzureModelKind; deployment: string}

export interface AzureTestInput {
  connectionId?: string
  config?: AzureConnectionConfigInput
  model?: AzureTestModelRef
}

export interface AzureTestResult {
  ok: boolean
  latencyMs: number
  error?: string
  code?: string
}

export const LOCAL_DEFAULT_CONTEXT_WINDOW = 8192
export const LOCAL_MIN_CONTEXT_WINDOW = 1024
export const LOCAL_MAX_CONTEXT_WINDOW = 2000000

export interface LocalModelsConfig {
  configured: boolean
  baseUrl: string
  contextWindow?: number
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
