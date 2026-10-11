import {
  AZURE_MODEL_KINDS,
  isKnown,
  type AzureDiscoveryItem,
  type AzureModelCreateInput,
  type AzureModelEntry,
  type AzureModelKind,
  type AzureModelUpdateInput,
  type ModelPurpose,
} from '@/types'
import {
  DEFAULT_PURPOSE,
  charLength,
  cleanPurposeText,
  normalizePurposeInput,
  resolvePurpose,
  type PurposeLabelOf,
} from '@/lib/modelPurpose'

export const DISPLAY_NAME_MAX_LENGTH = 64
export const DESCRIPTION_MAX_LENGTH = 200
export const CONTEXT_WINDOW_MIN = 1024
export const CONTEXT_WINDOW_MAX = 2_000_000
export const MAX_OUTPUT_MIN = 256
export const MAX_OUTPUT_MAX = 1_000_000

const DEPLOYMENT_NAME = /^[A-Za-z0-9._-]{1,64}$/
const AGENT_NAME = /^[\p{L}\p{M}\p{N}\p{P}\p{S} ]{1,128}$/u
const WHOLE_NUMBER = /^\d+$/

export function cleanModelText(text: string): string {
  return cleanPurposeText(text)
}

export function cleanLength(text: string): number {
  return charLength(cleanModelText(text))
}

export function defaultDisplayName(deployment: string): string {
  return [...deployment].slice(0, DISPLAY_NAME_MAX_LENGTH).join('')
}

export interface ModelFormState {
  connectionId: string
  kind: AzureModelKind
  deployment: string
  displayName: string
  displayNameEdited: boolean
  purpose: ModelPurpose
  purposeCustom: string
  description: string
  contextWindow: string
  maxOutputTokens: string
}

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

export function modelFormFromEntry(entry: AzureModelEntry): ModelFormState | null {
  if (!isKnown(AZURE_MODEL_KINDS, entry.kind)) return null
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

export function modelFormFromDiscovery(item: AzureDiscoveryItem, connectionId: string): ModelFormState {
  return {
    ...emptyModelForm(connectionId),
    kind: item.kind,
    deployment: item.name,
    displayName: defaultDisplayName(item.name),
  }
}

export function discoveryQuickAddInput(item: AzureDiscoveryItem, connectionId: string): AzureModelCreateInput {
  return {connectionId, kind: item.kind, deployment: item.name, purpose: DEFAULT_PURPOSE}
}

export function withDeployment(form: ModelFormState, deployment: string): ModelFormState {
  return form.displayNameEdited
    ? {...form, deployment}
    : {...form, deployment, displayName: defaultDisplayName(deployment)}
}

export function withDisplayName(form: ModelFormState, displayName: string): ModelFormState {
  return {...form, displayName, displayNameEdited: true}
}

export function inferKind(name: string): Extract<AzureModelKind, 'claude' | 'embedding'> | null {
  const n = name.toLowerCase()
  if (n.includes('embed')) return 'embedding'
  if (n.includes('claude')) return 'claude'
  return null
}

export function modelFormsDiffer(a: ModelFormState, b: ModelFormState): boolean {
  return (Object.keys(a) as (keyof ModelFormState)[]).some(k => k !== 'displayNameEdited' && a[k] !== b[k])
}

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

export type ModelFieldError = 'required' | 'tooLong' | 'invalidName' | 'kindNeedsProject' | 'notInteger' | 'outOfRange'

export interface ModelValidation {
  errors: Partial<Record<ModelFormField, ModelFieldError>>
  firstInvalid: ModelFormField | null
}

function numberError(text: string, min: number, max: number): ModelFieldError | null {
  const v = text.trim()
  if (!v) return null
  if (!WHOLE_NUMBER.test(v)) return 'notInteger'
  const n = Number(v)
  return n < min || n > max ? 'outOfRange' : null
}

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
