// AI provider catalogue + auth flows (OAuth device-code, GitHub Copilot/Models).

import type {Tolerant} from './tolerant'

export type ProviderID =
  'claude' | 'openai' | 'gemini' | 'xai' | 'glm' | 'github-models' | 'copilot' | 'azure-foundry' | 'local'

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
  local: {label: 'Local model'},
} as const satisfies Record<ProviderID, {label: string}>

export const PROVIDER_IDS: readonly ProviderID[] = Object.keys(PROVIDERS) as ProviderID[]

export const PROVIDER_OPTIONS: {value: ProviderID; label: string}[] = PROVIDER_IDS.map(id => ({
  value: id,
  label: PROVIDERS[id].label,
}))

export const AUTH_TYPES = ['api_key', 'oauth'] as const
export type AuthType = (typeof AUTH_TYPES)[number]

export interface ModelDetail {
  id: string
  displayName: string
  contextLimit: number
  inputCostPerM: number
  outputCostPerM: number
  /** Provider-family tool support (native or marker-based fallback). */
  supportsTools?: boolean
  // Azure AI Foundry registry entries (id "m/<entryId>") carry the fields
  // below; catalogue models leave them out. Embedding entries are never
  // listed in the picker, and neither is a kind this bundle does not know.
  kind?: Tolerant<AzureModelKind>
  purpose?: Tolerant<ModelPurpose>
  /** The text of a 'custom' purpose. */
  purposeCustom?: string
  /** Shown under the name in the chat picker. Admin-written: plain text only. */
  description?: string
  /** An organisation's entry shared with the caller, not the caller's own. */
  shared?: boolean
  connectionId?: string
  connectionName?: string
  /**
   * 'ready', or why the entry cannot be used right now (`statusReason`
   * carries the copy). A status this bundle does not know is unusable too.
   */
  status?: Tolerant<AzureStatus>
  statusReason?: string
  /**
   * Pre-registry ids this entry answers for (`gpt-4.1`, `claude:x`,
   * `agent:y`) — only entries of the resource `default` carry them; see
   * lib/modelIds findModel.
   */
  aliases?: string[]
  /** The server default for new chats (personal entries first, then the org's). */
  isDefault?: boolean
  /** The organisation's "Default for members". */
  isOrgDefault?: boolean
}

export interface ProviderInfo {
  /** A provider this bundle does not know is listed too; the chat leaves it out (it cannot send to it). */
  id: Tolerant<ProviderID>
  name: string
  authType: Tolerant<AuthType>
  contextLimit: number
  models: ModelDetail[]
  defaultModel: string
  configured: boolean
  /**
   * True when the credentials came from the organisation named by `orgId`
   * (GET /api/providers/list?orgId=… — the open flow's org, else the active
   * one) rather than the member. A member's own credentials always win; the
   * org's are the fallback, for its admins and members only.
   */
  shared?: boolean
  /**
   * Azure AI Foundry: the caller's migrated resource can list its project
   * and the "Your project has models you haven't added" prompt was not
   * dismissed — the chat picker offers "Review". Omitted when false.
   */
  suggestDiscovery?: boolean
}

export interface ProviderTestResult {
  ok: boolean
  latencyMs: number
  error?: string
}

// How baki signs in to an Azure AI Foundry resource: an API key, a Microsoft
// Entra ID app registration, or (desktop only) the Azure CLI's signed-in user.
export const AZURE_AUTH_MODES = ['api_key', 'entra', 'entra_cli'] as const
export type AzureFoundryAuthMode = (typeof AZURE_AUTH_MODES)[number]

/** A resource's credentials as sent. A missing secret keeps the stored one where the carry rules allow. */
export type AzureFoundryAuthInput =
  | {mode: 'api_key'; key?: string}
  | {mode: 'entra'; tenantId: string; clientId: string; clientSecret?: string}
  /** Azure CLI sign-in: the CLI's signed-in user is the credential — no client id or secret, just an optional tenant. */
  | {mode: 'entra_cli'; tenantId?: string}

// ---- Azure AI Foundry model registry: several resources, each with models ----
//
// The user's own registry lives under /api/providers/azure-foundry/…, an
// organisation's under /api/orgs/{orgId}/providers/azure-foundry/… (any
// admin or member reads; writes, tests and discovery need an admin). The API
// and the code call a resource a "connection"; the UI says "resource"
// (Foundry has a "Connections" concept of its own). Errors keep the usual
// envelope: branch on ApiError.code.

/** Whose registry a call addresses: the signed-in user's own, or an organisation's. */
export type AzureScope = {kind: 'user'} | {kind: 'org'; orgId: string}

/**
 * What a registry entry is: `chat` (OpenAI-compatible deployment), `claude`
 * (a Claude deployment on the Anthropic Messages route), `agent` (a Foundry
 * Agent Service agent — needs Entra ID + a project endpoint) or `embedding`
 * (never offered in the chat picker).
 */
export const AZURE_MODEL_KINDS = ['chat', 'claude', 'agent', 'embedding'] as const
export type AzureModelKind = (typeof AZURE_MODEL_KINDS)[number]
/** The kinds the chat talks to: not an embedding model, nor a kind this bundle does not know. */
export const CHAT_MODEL_KINDS = ['chat', 'claude', 'agent'] as const satisfies readonly AzureModelKind[]

/** The preset purposes, in picker order (lib/modelPurpose holds the runtime list). */
export type ModelPurposePreset = 'general' | 'documentation' | 'fixes' | 'review' | 'refactoring' | 'testing'
/** A stored purpose: a preset key, or 'custom' with its text in `purposeCustom`. */
export type ModelPurpose = ModelPurposePreset | 'custom'

/**
 * Computed by the server at read time, never stored. A resource is `ready`,
 * `needs_credentials` (missing secret, or its lender was disabled),
 * `unreadable` (the stored secret cannot be decrypted or parsed) or
 * `invalid_config` (the stored settings no longer validate, e.g. the
 * operator narrowed the allowed hosts). A model inherits its resource's
 * status, or is `unsupported` (an agent on a resource without Entra ID and a
 * project endpoint). `statusReason` carries user-readable copy.
 */
export const AZURE_STATUSES = ['ready', 'needs_credentials', 'unreadable', 'invalid_config', 'unsupported'] as const
export type AzureStatus = (typeof AZURE_STATUSES)[number]

/**
 * GET /connections `registryState`: `read_only` — the desktop list file was
 * written by a newer app version; `recovered` — a corrupt list file was
 * quarantined (a copy kept) and the registry started empty. A state this
 * bundle does not know reads as `ok`: the server refuses what it cannot take.
 */
export const AZURE_REGISTRY_STATES = ['ok', 'read_only', 'recovered'] as const
export type AzureRegistryState = (typeof AZURE_REGISTRY_STATES)[number]

/** Per-owner limits (10 resources, 50 models today). */
export interface AzureRegistryLimits {
  connections: number
  models: number
}

/** The last test of a saved resource or model (`at` is RFC 3339). */
export interface AzureLastTest {
  ok: boolean
  latencyMs?: number
  error?: string
  at: string
}

/**
 * A resource's public settings: never a secret, never deployment fields.
 * Filled only while the stored credential document can be read: without one
 * (status `needs_credentials` after its lender was disabled, or
 * `unreadable`) the server sends `{endpoint: '', authMode: '', hasSecret:
 * false}`.
 */
export interface AzureConnectionPublicConfig {
  endpoint: string
  /** '' when the stored document is missing or unreadable (see above). */
  authMode: Tolerant<AzureFoundryAuthMode> | ''
  /** A secret is stored; a PUT that leaves it out keeps it when the carry rules allow. */
  hasSecret: boolean
  tenantId?: string
  clientId?: string
  projectEndpoint?: string
}

/** The resource view (GET/POST/PUT /connections). */
export interface AzureConnection {
  /** `default` for the migrated single-document configuration, else [a-z0-9]{10}. Unique per owner only. */
  id: string
  name: string
  /** An organisation's resource: always true in org scope, and on the "Shared with you" entries of user scope. */
  shared: boolean
  orgId?: string
  orgName?: string
  /** The resource migrated from the single-document configuration. */
  legacy: boolean
  config: AzureConnectionPublicConfig
  status: Tolerant<AzureStatus>
  statusReason?: string
  /** Entra ID (or the Azure CLI) with a project endpoint: deployments and agents can be discovered. */
  canDiscover: boolean
  supportsAgents: boolean
  /** Show the "Found in your project" prompt (discoverable, not dismissed). */
  suggestDiscovery: boolean
  modelCount: number
  lastTest?: AzureLastTest | null
  updatedAt: string
  /** Org scope: the stored credential's stamp, the compare-and-swap token of a credential change. */
  credentialUpdatedAt?: string
  /** Org scope: who lent the credential. */
  sharedBy?: string
}

/** The model view (GET/POST/PUT /models). */
export interface AzureModelEntry {
  /** [a-z0-9]{10}, unique per owner; kept across resource, kind and deployment changes. */
  id: string
  /** The chat id: "m/<id>". */
  modelId: string
  connectionId: string
  connectionName: string
  /** A kind this bundle does not know is listed under a neutral label and cannot be edited here. */
  kind: Tolerant<AzureModelKind>
  /** The deployment name, or the agent name for kind 'agent'. */
  deployment: string
  /** What the server routes to: "<deployment>", "claude:<deployment>" or "agent:<name>". */
  target: string
  displayName: string
  /** An unknown purpose (a preset a newer server added) shows as custom text (lib/modelPurpose resolvePurpose). */
  purpose: Tolerant<ModelPurpose>
  purposeCustom?: string
  description?: string
  /** The user's override of the context window (1024–2,000,000); absent when unset. */
  contextWindow?: number
  /** The window the server knows from the catalogue, the served model or a learned limit. */
  reportedContextWindow?: number
  /** Caps a request's output (256–1,000,000); absent when unset. */
  maxOutputTokens?: number
  /** Org scope: the organisation's "Default for members". */
  isDefault: boolean
  /** The embedding model the knowledge base uses (one per owner). */
  isKnowledgeEmbedding: boolean
  shared: boolean
  orgId?: string
  orgName?: string
  status: Tolerant<AzureStatus>
  statusReason?: string
  lastTest?: AzureLastTest | null
  updatedAt: string
}

/** GET /connections. */
export interface AzureConnectionsListing {
  connections: AzureConnection[]
  registryState: Tolerant<AzureRegistryState>
  limits: AzureRegistryLimits
  /** The "Use for knowledge base" designation is offered (cloud with the Azure embedding provider). */
  knowledgeEmbeddingEnabled: boolean
  /**
   * With registryState 'recovered' (desktop): where the copy of the
   * unreadable list file was kept. The UI words it without when absent.
   */
  recoveredCopyPath?: string
}

/** GET /models. */
export interface AzureModelsListing {
  models: AzureModelEntry[]
  /**
   * User scope: the server's default for new chats ("m/<id>") — used when
   * the user saved no default of their own, so AI Accounts can mark it.
   */
  serverDefault?: string
}

/** One deployment or agent found by POST /connections/{id}/discover. */
export interface AzureDiscoveryItem {
  name: string
  /**
   * The kind it would be added as: chat, claude or embedding for a
   * deployment; agent for an agent. Closed: the parse leaves out an item of a
   * kind this bundle does not know, since it could not add one.
   */
  kind: AzureModelKind
  /** The underlying model (e.g. gpt-4.1 behind a deployment named "prod-chat"); '' when unknown. */
  model: string
  contextWindow?: number
  /**
   * Already in the registry on this resource — for a deployment, an entry of
   * any non-agent kind with that name; for an agent, an agent entry — and
   * `modelId` names it.
   */
  alreadyAdded: boolean
  modelId?: string
  /** Agents: the project deployment the agent runs on. */
  deployment?: string
  /** Agents: a workflow agent (a preview). */
  workflow?: boolean
}

/** POST /connections/{id}/discover. An Azure refusal arrives as a 200 with `error`. */
export interface AzureDiscovery {
  deployments: AzureDiscoveryItem[]
  agents: AzureDiscoveryItem[]
  warnings: string[]
  error?: string
}

/** A resource's settings as sent (a missing secret keeps the stored one where the carry rules allow). */
export interface AzureConnectionConfigInput {
  endpoint: string
  auth: AzureFoundryAuthInput
  projectEndpoint?: string
}

/** POST /connections. */
export interface AzureConnectionCreateInput {
  name: string
  config: AzureConnectionConfigInput
}

/** PUT /connections/{id}: every field optional; omitted ones stay as they are. */
export interface AzureConnectionUpdateInput {
  name?: string
  config?: AzureConnectionConfigInput
  /** The `updatedAt` last read (compare-and-swap: a stale value answers 409 CONFLICT). */
  updatedAt?: string
  /** The `credentialUpdatedAt` last read, sent with a credential change (org compare-and-swap). */
  credentialUpdatedAt?: string
  /** Hide the "Found in your project" prompt for good. */
  dismissDiscovery?: boolean
  /**
   * Confirmed re-index: the resource holds the knowledge-base model of a
   * non-empty corpus and its endpoint changes (409 REINDEX_REQUIRED otherwise).
   */
  force?: boolean
}

/** DELETE /connections/{id} query flags. */
export interface AzureConnectionDeleteOptions {
  /** Delete its models too (without it, a resource with models answers 409 CONNECTION_IN_USE). */
  cascade?: boolean
  /** Confirmed re-index: the resource holds the knowledge-base model (else 409 REINDEX_REQUIRED). */
  force?: boolean
}

/** DELETE /connections/{id}. */
export interface AzureConnectionDeleteResult {
  status: string
  deletedModels: number
  clearedKnowledgeEmbedding: boolean
}

/** POST /models. */
export interface AzureModelCreateInput {
  connectionId: string
  kind: AzureModelKind
  deployment: string
  /** Omitted: the deployment name. */
  displayName?: string
  /** Omitted: 'general'. */
  purpose?: ModelPurpose
  /** Required with purpose 'custom' (1–32 characters). */
  purposeCustom?: string
  description?: string
  contextWindow?: number
  maxOutputTokens?: number
  /** Org scope only: make it the "Default for members". */
  isDefault?: boolean
  isKnowledgeEmbedding?: boolean
  /** Confirmed re-index after a 409 REINDEX_REQUIRED. */
  force?: boolean
}

/**
 * PUT /models/{id}: omitted fields stay as they are; `null` clears an
 * optional number (and '' the description).
 */
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
  /**
   * The `updatedAt` last read (compare-and-swap: a stale value answers 409
   * CONFLICT). The Model modal's edit sends it; the one-click designations
   * leave it out.
   */
  updatedAt?: string
  force?: boolean
}

/** DELETE /models/{id} query flags. */
export interface AzureModelDeleteOptions {
  /** Confirmed re-index: it is the knowledge-base model of a non-empty corpus. */
  force?: boolean
}

/** A saved model (its entry `id`), or a draft on the test's resource. */
export type AzureTestModelRef = {id: string} | {kind: AzureModelKind; deployment: string}

/**
 * POST /test — one route for a saved or draft resource and a saved or draft
 * model: `{connectionId}` tests a saved resource, `{config}` a draft one
 * (with `connectionId` too, the stored secret carries over), and `model`
 * adds a model test on that resource. A resource test makes no model call;
 * a chat, Claude or embedding test sends one tiny billed request; an agent
 * test only checks the agent exists.
 */
export interface AzureTestInput {
  connectionId?: string
  config?: AzureConnectionConfigInput
  model?: AzureTestModelRef
}

/** POST /test: a 200 for every test outcome; a 4xx only for an invalid request. */
export interface AzureTestResult {
  ok: boolean
  latencyMs: number
  error?: string
  code?: string
}

// The generic local OpenAI-compatible provider (Foundry Local, Ollama, LM
// Studio…) is configured with a base URL — no key — and an optional context
// window via /api/providers/local/config. Desktop only: a cloud server answers
// 400 LOCAL_MODELS_UNAVAILABLE to the GET and the POST. Removal and connection
// tests reuse the generic key endpoints with provider id 'local'.

/** Context window (tokens) the server assumes when none is saved. */
export const LOCAL_DEFAULT_CONTEXT_WINDOW = 8192
/** Bounds of a saved context window (the server answers INVALID_CONTEXT_WINDOW outside them). */
export const LOCAL_MIN_CONTEXT_WINDOW = 1024
export const LOCAL_MAX_CONTEXT_WINDOW = 2000000

/** GET /api/providers/local/config. */
export interface LocalModelsConfig {
  configured: boolean
  /** The saved base URL, stored without the /v1 suffix (e.g. http://127.0.0.1:11434); empty when not configured. */
  baseUrl: string
  /** The saved context window in tokens; absent when unset (LOCAL_DEFAULT_CONTEXT_WINDOW applies). */
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
