import type {AzureModelKind} from '@/types'

export const REGISTRY_MODEL_PREFIX = 'm/'
export const LEGACY_AGENT_PREFIX = 'agent:'
export const LEGACY_CLAUDE_PREFIX = 'claude:'

const REGISTRY_MODEL_ID = /^m\/[a-z0-9]{10}$/

export function isRegistryModelId(id: string | null | undefined): boolean {
  return typeof id === 'string' && REGISTRY_MODEL_ID.test(id)
}

export function registryEntryId(modelId: string | null | undefined): string | null {
  return isRegistryModelId(modelId) ? (modelId as string).slice(REGISTRY_MODEL_PREFIX.length) : null
}

export function registryModelId(entryId: string): string {
  return `${REGISTRY_MODEL_PREFIX}${entryId}`
}

export interface IdentifiableModel {
  id: string
  aliases?: readonly string[] | null
  shared?: boolean
}

export function hasAlias(model: IdentifiableModel, id: string): boolean {
  if (!model.aliases || model.aliases.length === 0) return false
  const want = id.toLowerCase()
  return model.aliases.some(a => a.toLowerCase() === want)
}

export function resolveAlias<M extends IdentifiableModel>(id: string, models: readonly M[]): M | undefined {
  if (!id) return undefined
  return models.find(m => !m.shared && hasAlias(m, id)) ?? models.find(m => m.shared && hasAlias(m, id))
}

export function findModel<M extends IdentifiableModel>(id: string, models: readonly M[]): M | undefined {
  if (!id) return undefined
  return models.find(m => m.id === id) ?? resolveAlias(id, models)
}

export function storableModelLabel(
  label: string | null | undefined,
  model: string | null | undefined,
): string | undefined {
  if (!label || !label.trim() || label === model || isRegistryModelId(label)) return undefined
  return label
}

export function modelNameOf(
  model: string | null | undefined,
  label: string | null | undefined,
  listed: readonly (IdentifiableModel & {displayName?: string})[] = [],
): string | null {
  const kept = storableModelLabel(label, model)
  if (kept) return kept
  if (!model) return ''
  const entry = findModel(model, listed)
  if (entry) return entry.displayName || entry.id
  return isRegistryModelId(model) ? null : model
}

export function modelKind(id: string | null | undefined, detail?: {kind?: string} | null): AzureModelKind | undefined {
  const kind = detail?.kind
  if (kind === 'chat' || kind === 'claude' || kind === 'agent' || kind === 'embedding') return kind
  if (!id) return undefined
  if (id.startsWith(LEGACY_AGENT_PREFIX)) return 'agent'
  if (id.startsWith(LEGACY_CLAUDE_PREFIX)) return 'claude'
  return undefined
}

export function isAgentModel(id: string | null | undefined, detail?: {kind?: string} | null): boolean {
  return modelKind(id, detail) === 'agent'
}
