import type {
  AzureDiscoveryItem,
  AzureModelCreateInput,
  AzureModelEntry,
  AzureModelKind,
  AzureModelUpdateInput,
  ModelPurpose,
} from '@/types'
import {
  DEFAULT_PURPOSE,
  charLength,
  cleanPurposeText,
  normalizePurposeInput,
  resolvePurpose,
  type PurposeLabelOf,
} from '@/lib/modelPurpose'

// The Model modal's pure half: its form state, validation (mirroring the
// server's rules), the request bodies, the kind hint and where a server
// error code belongs.

export const DISPLAY_NAME_MAX_LENGTH = 64
export const DESCRIPTION_MAX_LENGTH = 200
export const CONTEXT_WINDOW_MIN = 1024
export const CONTEXT_WINDOW_MAX = 2_000_000
export const MAX_OUTPUT_MIN = 256
export const MAX_OUTPUT_MAX = 1_000_000

// An Azure deployment name (the server's azureDeploymentName rule).
const DEPLOYMENT_NAME = /^[A-Za-z0-9._-]{1,64}$/
// An agent name: 1–128 printable characters — letters, marks, digits,
// punctuation, symbols and the plain space (Go's unicode.IsPrint).
const AGENT_NAME = /^[\p{L}\p{M}\p{N}\p{P}\p{S} ]{1,128}$/u
const WHOLE_NUMBER = /^\d+$/

/**
 * Display-name and description text as the server stores and counts it
 * (cleanAzureText): NFC-normalised, whitespace runs collapsed to one space,
 * trimmed — the cleaning a custom purpose gets too.
 */
export function cleanModelText(text: string): string {
  return cleanPurposeText(text)
}

/** Length in characters of the text as the server will store it (the counters and the limits). */
export function cleanLength(text: string): number {
  return charLength(cleanModelText(text))
}

/**
 * The display name a model takes when none is typed: its deployment (or
 * agent) name cut to the 64 characters a display name may have — what the
 * server stores for a blank one. Agent names run to 128.
 */
export function defaultDisplayName(deployment: string): string {
  return [...deployment].slice(0, DISPLAY_NAME_MAX_LENGTH).join('')
}

export interface ModelFormState {
  /** The resource the model belongs to. */
  connectionId: string
  kind: AzureModelKind
  /** The deployment name, or the agent name for kind 'agent'. */
  deployment: string
  displayName: string
  /** The user typed a display name: it no longer follows the deployment name. */
  displayNameEdited: boolean
  purpose: ModelPurpose
  /** The text of a 'custom' purpose, as typed. */
  purposeCustom: string
  description: string
  /** Advanced overrides, as typed ('' = not set). Not for embeddings. */
  contextWindow: string
  maxOutputTokens: string
}

/** A new model on `connectionId`: a chat deployment for General. */
export function emptyModelForm(connectionId = ''): ModelFormState {
  return {
    connectionId,
    kind: 'chat',
    deployment: '',
    displayName: '',
    displayNameEdited: false,
    purpose: DEFAULT_PURPOSE,
    purposeCustom: '',
    description: '',
    contextWindow: '',
    maxOutputTokens: '',
  }
}

/** The edit form of a saved model. A display name that is the deployment's default keeps following it. */
export function modelFormFromEntry(entry: AzureModelEntry): ModelFormState {
  const purpose = resolvePurpose(entry)
  return {
    connectionId: entry.connectionId,
    kind: entry.kind,
    deployment: entry.deployment,
    displayName: entry.displayName,
    displayNameEdited: entry.displayName !== cleanModelText(defaultDisplayName(entry.deployment)),
    purpose: purpose.kind === 'preset' ? purpose.preset : 'custom',
    purposeCustom: purpose.kind === 'custom' ? purpose.text : '',
    description: entry.description ?? '',
    contextWindow: entry.contextWindow ? String(entry.contextWindow) : '',
    maxOutputTokens: entry.maxOutputTokens ? String(entry.maxOutputTokens) : '',
  }
}

/** "Add with details…" on a discovered deployment or agent: pre-filled, the window left to the reported one. */
export function modelFormFromDiscovery(item: AzureDiscoveryItem, connectionId: string): ModelFormState {
  return {
    ...emptyModelForm(connectionId),
    kind: item.kind,
    deployment: item.name,
    displayName: defaultDisplayName(item.name),
  }
}

/**
 * Quick add of a discovered item: the defaults — purpose General, and no
 * display name, so the server gives it the name (cut to 64 characters: an
 * agent's name may run to 128, a display name may not).
 */
export function discoveryQuickAddInput(item: AzureDiscoveryItem, connectionId: string): AzureModelCreateInput {
  return {connectionId, kind: item.kind, deployment: item.name, purpose: DEFAULT_PURPOSE}
}

/** Sets the deployment name; the display name follows it (defaultDisplayName) until the user edits the display name. */
export function withDeployment(form: ModelFormState, deployment: string): ModelFormState {
  return form.displayNameEdited
    ? {...form, deployment}
    : {...form, deployment, displayName: defaultDisplayName(deployment)}
}

/** Sets the display name as typed; it stops following the deployment. */
export function withDisplayName(form: ModelFormState, displayName: string): ModelFormState {
  return {...form, displayName, displayNameEdited: true}
}

/**
 * The kind a deployment name suggests — "embed" → embedding, "claude" →
 * claude — or null. A hint only: the modal offers the switch, it never
 * switches by itself.
 */
export function inferKind(name: string): Extract<AzureModelKind, 'claude' | 'embedding'> | null {
  const n = name.toLowerCase()
  if (n.includes('embed')) return 'embedding'
  if (n.includes('claude')) return 'claude'
  return null
}

/** True when any value differs from the baseline — the dirty check before discarding. */
export function modelFormsDiffer(a: ModelFormState, b: ModelFormState): boolean {
  return (Object.keys(a) as (keyof ModelFormState)[]).some(k => k !== 'displayNameEdited' && a[k] !== b[k])
}

/** The modal's fields in tab order (the first invalid one takes focus). */
export type ModelFormField =
  | 'connectionId'
  | 'kind'
  | 'deployment'
  | 'displayName'
  | 'purpose'
  | 'description'
  | 'contextWindow'
  | 'maxOutputTokens'

export const MODEL_FIELD_ORDER: readonly ModelFormField[] = [
  'connectionId',
  'kind',
  'deployment',
  'displayName',
  'purpose',
  'description',
  'contextWindow',
  'maxOutputTokens',
]

/**
 * A field's problem. `invalidName`: not a deployment name
 * (^[A-Za-z0-9._-]{1,64}$) or, for an agent, not 1–128 printable
 * characters. `kindNeedsProject`: an agent on a resource without Entra ID
 * and a project endpoint. `notInteger` / `outOfRange`: the advanced numbers
 * (context window 1024–2,000,000; max output 256–1,000,000).
 */
export type ModelFieldError = 'required' | 'tooLong' | 'invalidName' | 'kindNeedsProject' | 'notInteger' | 'outOfRange'

export interface ModelValidation {
  errors: Partial<Record<ModelFormField, ModelFieldError>>
  /** Where focus goes on submit; null when the form may be sent. */
  firstInvalid: ModelFormField | null
}

function numberError(text: string, min: number, max: number): ModelFieldError | null {
  const v = text.trim()
  if (!v) return null
  if (!WHOLE_NUMBER.test(v)) return 'notInteger'
  const n = Number(v)
  return n < min || n > max ? 'outOfRange' : null
}

/**
 * Validates on submit (the modal keeps Save enabled). `supportsAgents` is
 * the chosen resource's; `existing` is the saved model an edit started from;
 * `labelOf` lets custom text naming a preset in the UI language count as
 * that preset. Lengths count the text as the server stores it (cleanLength).
 */
export function validateModelForm(
  form: ModelFormState,
  opts: {
    supportsAgents?: boolean
    existing?: Pick<AzureModelEntry, 'kind' | 'connectionId'> | null
    labelOf?: PurposeLabelOf
  } = {},
): ModelValidation {
  const errors: ModelValidation['errors'] = {}
  if (!form.connectionId) errors.connectionId = 'required'
  // An agent on a resource that can't run one (it lost Entra ID or its
  // project) stays editable in place — the server refuses only a change of
  // kind or resource that would put an agent there.
  const agentKept = opts.existing?.kind === 'agent' && opts.existing.connectionId === form.connectionId
  if (form.kind === 'agent' && opts.supportsAgents === false && !agentKept) errors.kind = 'kindNeedsProject'

  const name = form.deployment.trim()
  if (!name) errors.deployment = 'required'
  else if (!(form.kind === 'agent' ? AGENT_NAME : DEPLOYMENT_NAME).test(name)) errors.deployment = 'invalidName'

  if (cleanLength(form.displayName) > DISPLAY_NAME_MAX_LENGTH) errors.displayName = 'tooLong'

  const purpose = normalizePurposeInput(form.purpose, form.purposeCustom, opts.labelOf)
  if (!purpose.ok) errors.purpose = purpose.error

  if (cleanLength(form.description) > DESCRIPTION_MAX_LENGTH) errors.description = 'tooLong'

  if (form.kind !== 'embedding') {
    const window = numberError(form.contextWindow, CONTEXT_WINDOW_MIN, CONTEXT_WINDOW_MAX)
    if (window) errors.contextWindow = window
    const output = numberError(form.maxOutputTokens, MAX_OUTPUT_MIN, MAX_OUTPUT_MAX)
    if (output) errors.maxOutputTokens = output
  }

  return {errors, firstInvalid: MODEL_FIELD_ORDER.find(f => errors[f]) ?? null}
}

function purposeFields(
  form: ModelFormState,
  labelOf?: PurposeLabelOf,
): Pick<AzureModelCreateInput, 'purpose' | 'purposeCustom'> {
  const purpose = normalizePurposeInput(form.purpose, form.purposeCustom, labelOf)
  if (!purpose.ok) return {}
  return purpose.purpose === 'custom'
    ? {purpose: 'custom', purposeCustom: purpose.purposeCustom}
    : {purpose: purpose.purpose}
}

function optionalNumber(text: string): number | undefined {
  const v = text.trim()
  return WHOLE_NUMBER.test(v) ? Number(v) : undefined
}

/**
 * POST /models from a validated form: trimmed, blank optional fields left
 * out (the server then uses the deployment name and General), the advanced
 * numbers only for non-embedding kinds.
 */
export function toModelInput(form: ModelFormState, labelOf?: PurposeLabelOf): AzureModelCreateInput {
  const body: AzureModelCreateInput = {
    connectionId: form.connectionId,
    kind: form.kind,
    deployment: form.deployment.trim(),
    ...purposeFields(form, labelOf),
  }
  const displayName = form.displayName.trim()
  if (displayName) body.displayName = displayName
  const description = form.description.trim()
  if (description) body.description = description
  if (form.kind !== 'embedding') {
    const contextWindow = optionalNumber(form.contextWindow)
    if (contextWindow !== undefined) body.contextWindow = contextWindow
    const maxOutputTokens = optionalNumber(form.maxOutputTokens)
    if (maxOutputTokens !== undefined) body.maxOutputTokens = maxOutputTokens
  }
  return body
}

/**
 * PUT /models/{id} from a validated edit form: every editable field, so a
 * cleared one is cleared on the server too — '' for the description, null
 * for the advanced numbers (always null for an embedding), and the
 * deployment's default for a blank display name. `updatedAt` is the edited
 * model's as loaded: a change saved meanwhile answers 409 CONFLICT instead
 * of being overwritten.
 */
export function toModelUpdateInput(
  form: ModelFormState,
  labelOf?: PurposeLabelOf,
  updatedAt?: string,
): AzureModelUpdateInput {
  const deployment = form.deployment.trim()
  const embedding = form.kind === 'embedding'
  return {
    connectionId: form.connectionId,
    kind: form.kind,
    deployment,
    displayName: form.displayName.trim() || defaultDisplayName(deployment),
    ...purposeFields(form, labelOf),
    description: form.description.trim(),
    contextWindow: embedding ? null : (optionalNumber(form.contextWindow) ?? null),
    maxOutputTokens: embedding ? null : (optionalNumber(form.maxOutputTokens) ?? null),
    ...(updatedAt ? {updatedAt} : {}),
  }
}

/**
 * Where a failed save or test goes: a field, the re-index confirm
 * (`reindex`), the read-only copy (`forbidden`, a 403), or the form-level
 * alert (`form` — TOO_MANY_MODELS, CONFLICT, 5xx and anything unknown).
 */
export type ModelErrorTarget = ModelFormField | 'reindex' | 'forbidden' | 'form'

export function modelErrorTarget(err: unknown): ModelErrorTarget {
  const e = (err ?? {}) as {code?: unknown; status?: unknown}
  if (e.status === 403) return 'forbidden'
  switch (e.code) {
    case 'INVALID_DEPLOYMENT':
    case 'DUPLICATE_MODEL':
      return 'deployment'
    case 'INVALID_KIND':
    case 'KIND_REQUIRES_PROJECT':
      return 'kind'
    case 'INVALID_DISPLAY_NAME':
      return 'displayName'
    case 'INVALID_PURPOSE':
      return 'purpose'
    case 'INVALID_DESCRIPTION':
      return 'description'
    case 'INVALID_CONTEXT_WINDOW':
      return 'contextWindow'
    case 'INVALID_MAX_OUTPUT':
      return 'maxOutputTokens'
    case 'CONNECTION_NOT_FOUND':
      return 'connectionId'
    case 'REINDEX_REQUIRED':
      return 'reindex'
    default:
      return 'form'
  }
}
