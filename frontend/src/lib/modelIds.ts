import type {AzureModelKind} from '@/types'

// Model ids the chat sends and how older ones map onto today's.
//
// An Azure AI Foundry registry entry is sent as "m/<entryId>" (entryId is
// [a-z0-9]{10}; a deployment name can never contain "/"). Before the
// registry, Azure models were sent by their routing id — the deployment name,
// "claude:<deployment>" or "agent:<name>". Those ids still sit in saved
// defaults and old messages; the entries of the migrated resource `default`
// list them as `aliases`, so they resolve here (personal entries first, then
// the shared ones — the server only exposes shared aliases to members without
// a personal `default` resource).

export const REGISTRY_MODEL_PREFIX = 'm/'
/** The pre-registry routing prefixes of Azure agent and Claude deployments. */
export const LEGACY_AGENT_PREFIX = 'agent:'
export const LEGACY_CLAUDE_PREFIX = 'claude:'

const REGISTRY_MODEL_ID = /^m\/[a-z0-9]{10}$/

/** True for an Azure registry model id ("m/" + 10 lower-case letters or digits). */
export function isRegistryModelId(id: string | null | undefined): boolean {
  return typeof id === 'string' && REGISTRY_MODEL_ID.test(id)
}

/** The entry id inside a registry model id ("m/ab12cd34ef" → "ab12cd34ef"), or null. */
export function registryEntryId(modelId: string | null | undefined): string | null {
  return isRegistryModelId(modelId) ? (modelId as string).slice(REGISTRY_MODEL_PREFIX.length) : null
}

/** The chat id of a registry entry. */
export function registryModelId(entryId: string): string {
  return `${REGISTRY_MODEL_PREFIX}${entryId}`
}

/** The fields the lookups below read; ModelDetail and the registry views fit. */
export interface IdentifiableModel {
  id: string
  aliases?: readonly string[] | null
  shared?: boolean
}

/** True when `model` answers for the pre-registry id `id` (compared case-insensitively, as Azure names are). */
export function hasAlias(model: IdentifiableModel, id: string): boolean {
  if (!model.aliases || model.aliases.length === 0) return false
  const want = id.toLowerCase()
  return model.aliases.some(a => a.toLowerCase() === want)
}

/**
 * The listed model a pre-registry id stands for, through `aliases`: the
 * caller's own entry first, then a shared one; undefined when none answers.
 */
export function resolveAlias<M extends IdentifiableModel>(id: string, models: readonly M[]): M | undefined {
  if (!id) return undefined
  return models.find(m => !m.shared && hasAlias(m, id)) ?? models.find(m => m.shared && hasAlias(m, id))
}

/** The listed model `id` names: itself when listed, else through aliases (see resolveAlias). */
export function findModel<M extends IdentifiableModel>(id: string, models: readonly M[]): M | undefined {
  if (!id) return undefined
  return models.find(m => m.id === id) ?? resolveAlias(id, models)
}

/**
 * The label worth keeping with a stored model reference (a message's, a
 * thread's pin, a turn's): its friendly name. undefined for none, for the id
 * itself (it adds nothing, and would keep the listing's name from ever
 * showing) and for a registry id ("m/…"), which names nothing a reader knows.
 */
export function storableModelLabel(
  label: string | null | undefined,
  model: string | null | undefined,
): string | undefined {
  if (!label || !label.trim() || label === model || isRegistryModelId(label)) return undefined
  return label
}

/**
 * The name a stored model reference (a message's or a thread's) goes by: the
 * label it was saved with, else the listing's display name (through
 * aliases), else the id itself — except a registry id that nothing lists and
 * no label names, which has no name left: null, and the caller says
 * "removed model". '' when there is no model at all. A label that is only an
 * id (see storableModelLabel) counts as none.
 */
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

/**
 * A model's kind: the listing's `kind` when it has one, else what a legacy
 * routing prefix says ("agent:" → agent, "claude:" → claude); undefined
 * when neither tells (a plain deployment or a catalogue model).
 */
export function modelKind(id: string | null | undefined, detail?: {kind?: string} | null): AzureModelKind | undefined {
  const kind = detail?.kind
  if (kind === 'chat' || kind === 'claude' || kind === 'agent' || kind === 'embedding') return kind
  if (!id) return undefined
  if (id.startsWith(LEGACY_AGENT_PREFIX)) return 'agent'
  if (id.startsWith(LEGACY_CLAUDE_PREFIX)) return 'claude'
  return undefined
}

/**
 * Whether a model is a Foundry agent (which runs its own tools, so the chat's
 * tool loop is off): the listing's kind wins, and a model without one — a
 * pre-registry id, or before the listing loaded — falls back to the legacy
 * "agent:" prefix.
 */
export function isAgentModel(id: string | null | undefined, detail?: {kind?: string} | null): boolean {
  return modelKind(id, detail) === 'agent'
}
