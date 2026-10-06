import type {AzureStatus} from '@/types'
import {findModel, hasAlias, isRegistryModelId, storableModelLabel} from './modelIds'

// Which model a chat thread uses — the pure half of per-thread model memory.
//
// A thread stores {provider, model, modelLabel} once pinned (its first
// accepted send, a header pick, or dismissing a removal notice). An unpinned
// thread with history predates per-thread memory: it takes its last
// assistant message's pair ("legacy"), provided that pair is still offered.
// Anything else starts on the active provider's "Default for new chats"
// (ai.providers[p].defaultModel), else the server's default. Nothing here
// writes settings: a fallback is per thread, and `activeProvider` stays the
// provider for new chats.
//
// A pinned or legacy model that can't be used falls back — to its own
// provider's default when that provider still answers here, else to the
// new-chat default — with a notice:
//   - removed (fallbackRemoved): the listing no longer offers it. Dismissing
//     the notice pins the thread to the fallback (the caller's job).
//   - unavailable (fallbackUnavailable): its resource needs credentials, is
//     unreadable or misconfigured, the model is unsupported there, or its
//     provider isn't configured in this scope. The thread is never re-pinned,
//     so it returns to the model once that is fixed. A legacy thread is
//     pinned to its OWN model then: unpinned, the stand-in's first reply
//     would become its last reply, and inference would adopt the stand-in.
// Both are decided only once this scope's listing has LOADED: while it loads
// or after it failed, the thread keeps its own pair and no notice shows. Nor
// is anything decided for a configured provider that lists nothing to chat
// with — a local server that is down, an Azure AI Foundry scope whose legacy
// import is still running: an empty listing says nothing about the model.

/**
 * Providers whose listing is complete: a model it doesn't list cannot be
 * used. A catalogue provider (Claude, OpenAI, …) may still serve an id it
 * doesn't list — a custom "Default for new chats" — so there only a legacy
 * pair it neither lists nor defaults to counts as removed.
 */
const COMPLETE_LISTING = new Set(['azure-foundry', 'local'])

/** The model fields selection reads; ModelDetail fits. */
export interface SelectableModel {
  id: string
  displayName?: string
  kind?: string
  status?: AzureStatus
  statusReason?: string
  aliases?: readonly string[] | null
  shared?: boolean
}

/** A provider as listed for this scope (ProviderInfo and the chat's ProviderOption fit). */
export interface SelectableProvider {
  id: string
  configured: boolean
  /** The server's default model for this scope. */
  defaultModel: string
  models: readonly SelectableModel[]
}

/** A thread's stored model. */
export interface ThreadModelPin {
  provider: string
  model: string
  modelLabel?: string
}

/** The message fields legacy inference reads; ChatMessage fits. */
export interface SelectionMessage {
  role: string
  provider?: string
  model?: string
  modelLabel?: string
}

/** The settings selection reads; AISettings fits. */
export interface SelectionSettings {
  activeProvider: string
  providers: Partial<Record<string, {defaultModel?: string} | undefined>>
}

export interface ModelSelectionInput {
  /** The thread's pin; absent (or incomplete) for an unpinned thread. */
  pin?: Partial<ThreadModelPin> | null
  /** The thread's messages, oldest first (legacy inference). */
  messages?: readonly SelectionMessage[]
  settings: SelectionSettings
  /** This scope's provider listing. */
  providers: readonly SelectableProvider[]
  /** The listing loaded (false while loading and after a failure). */
  listingLoaded: boolean
}

/**
 * pinned — the thread's own pin. default — the provider's saved "Default for
 * new chats". serverDefault — the server's default (or, with an unusable
 * one, the first usable listed model; model '' when there is none). legacy —
 * inferred from an unpinned thread's last assistant message.
 * fallbackRemoved / fallbackUnavailable — see the header comment.
 */
export type ModelSelectionReason =
  'pinned' | 'default' | 'serverDefault' | 'legacy' | 'fallbackRemoved' | 'fallbackUnavailable'

export interface ModelTarget {
  provider: string
  /** '' when nothing usable is offered (the composer explains; a send would get the server default). */
  model: string
  label: string
}

/** Why an unavailable model can't be used: its entry's status, or its provider isn't configured here. */
export type UnavailableCause = Exclude<AzureStatus, 'ready'> | 'provider'

/** What ModelUnavailableNotice shows. */
export interface ModelNoticeData {
  kind: 'removed' | 'unavailable'
  /** The model the thread asked for (its label as last known). */
  wanted: ModelTarget
  /** What the thread uses meanwhile; null when nothing is usable. */
  fallback: ModelTarget | null
  /** unavailable only. */
  cause?: UnavailableCause
  /** The server's copy for an entry's status, when it sent one. */
  statusReason?: string
}

export interface ModelSelection extends ModelTarget {
  reason: ModelSelectionReason
  /**
   * Write this pin to the thread (once): a legacy thread's inferred pair
   * (also while that model is only temporarily unavailable), or a pin whose
   * legacy id resolved to its "m/" entry or whose entry was renamed. Its
   * label is only ever a name, never an id (see storableModelLabel).
   */
  pin?: ThreadModelPin
  /** Set with the two fallback reasons. */
  notice?: ModelNoticeData
}

type DefaultTarget = ModelTarget & {reason: 'default' | 'serverDefault'}

function isUsable(model: SelectableModel): boolean {
  return !model.status || model.status === 'ready'
}

function selectable(provider: SelectableProvider): SelectableModel[] {
  return provider.models.filter(m => m.kind !== 'embedding')
}

function labelOf(model: SelectableModel): string {
  return model.displayName || model.id
}

// The name a pair taken as it is shows (the listing has not named it): its
// label, but never a registry id ("m/…"), which names nothing a reader
// knows — '' until the listing says (a turn sent meanwhile stores no label).
function shownLabel(label: string): string {
  return isRegistryModelId(label) ? '' : label
}

// The pin a target is stored as: its label only when it names more than the
// id (see storableModelLabel).
function pinOf(target: ModelTarget): ThreadModelPin {
  const modelLabel = storableModelLabel(target.label, target.model)
  return {provider: target.provider, model: target.model, ...(modelLabel ? {modelLabel} : {})}
}

function savedDefault(input: ModelSelectionInput, providerId: string): string {
  return input.settings.providers[providerId]?.defaultModel?.trim() ?? ''
}

function configuredProvider(input: ModelSelectionInput, providerId: string): SelectableProvider | undefined {
  return input.providers.find(p => p.id === providerId && p.configured)
}

// A model id a catalogue provider serves without listing it: its saved or
// server default.
function servesUnlisted(input: ModelSelectionInput, provider: SelectableProvider, model: string): boolean {
  if (COMPLETE_LISTING.has(provider.id)) return false
  return model === savedDefault(input, provider.id) || model === provider.defaultModel.trim()
}

/**
 * The model a provider starts a chat on in this scope: its saved "Default for
 * new chats" (resolved through aliases), else the server default, else — for
 * a complete listing whose default is gone or unusable — its first usable
 * model. Null when it offers nothing usable.
 */
function providerDefault(input: ModelSelectionInput, provider: SelectableProvider): DefaultTarget | null {
  const models = selectable(provider)
  const candidates: [string, DefaultTarget['reason']][] = [
    [savedDefault(input, provider.id), 'default'],
    [provider.defaultModel.trim(), 'serverDefault'],
  ]
  for (const [id, reason] of candidates) {
    if (!id) continue
    const entry = findModel(id, models)
    if (entry) {
      if (isUsable(entry)) return {provider: provider.id, model: entry.id, label: labelOf(entry), reason}
    } else if (!COMPLETE_LISTING.has(provider.id)) {
      return {provider: provider.id, model: id, label: id, reason}
    }
  }
  const first = models.find(isUsable)
  return first ? {provider: provider.id, model: first.id, label: labelOf(first), reason: 'serverDefault'} : null
}

/**
 * The model a provider's new chats start on in this scope — the picker's
 * [DEFAULT] — or null when it offers nothing usable (see providerDefault).
 */
export function providerNewChatDefault(
  settings: SelectionSettings,
  provider: SelectableProvider,
): (ModelTarget & {reason: 'default' | 'serverDefault'}) | null {
  return providerDefault({settings, providers: [provider], listingLoaded: true}, provider)
}

// A new (or unpinned, history-less) thread: the active provider when it is
// configured here, else the first configured one — per thread, no settings
// write.
function newChatSelection(input: ModelSelectionInput): ModelSelection {
  const {activeProvider} = input.settings
  if (!input.listingLoaded) {
    // The saved default as it is; a registry id has no name until the
    // listing says (a turn sent meanwhile must not store "m/…" as its label).
    const saved = savedDefault(input, activeProvider)
    return saved
      ? {provider: activeProvider, model: saved, label: shownLabel(saved), reason: 'default'}
      : {provider: activeProvider, model: '', label: '', reason: 'serverDefault'}
  }
  const provider = configuredProvider(input, activeProvider) ?? input.providers.find(p => p.configured)
  if (!provider) return {provider: activeProvider, model: '', label: '', reason: 'serverDefault'}
  return providerDefault(input, provider) ?? {provider: provider.id, model: '', label: '', reason: 'serverDefault'}
}

function fallback(
  input: ModelSelectionInput,
  wanted: ModelTarget,
  kind: 'removed' | 'unavailable',
  detail: {cause?: UnavailableCause; statusReason?: string} = {},
): ModelSelection {
  const own = configuredProvider(input, wanted.provider)
  const sameProvider = own ? providerDefault(input, own) : null
  const newChat = sameProvider ? null : newChatSelection(input)
  const target: ModelTarget | null = sameProvider ?? (newChat && newChat.model ? newChat : null)
  const notice: ModelNoticeData = {
    kind,
    wanted,
    fallback: target ? {provider: target.provider, model: target.model, label: target.label} : null,
    ...(detail.cause ? {cause: detail.cause} : {}),
    ...(detail.statusReason ? {statusReason: detail.statusReason} : {}),
  }
  const reason = kind === 'removed' ? 'fallbackRemoved' : 'fallbackUnavailable'
  return target
    ? {provider: target.provider, model: target.model, label: target.label, reason, notice}
    : {provider: wanted.provider, model: '', label: '', reason, notice}
}

// A pinned or legacy model that can't be used right now: the thread uses a
// stand-in meanwhile, with the notice, and keeps its own model. A legacy
// thread is pinned to it here — its entry's id when listed — or the
// stand-in's first reply would become its last reply, and legacy inference
// would adopt the stand-in for good.
function unavailable(
  input: ModelSelectionInput,
  wanted: ModelTarget,
  reason: 'pinned' | 'legacy',
  entry: SelectableModel | undefined,
  detail: {cause: UnavailableCause; statusReason?: string},
): ModelSelection {
  const sel = fallback(input, wanted, 'unavailable', detail)
  if (reason !== 'legacy') return sel
  return {...sel, pin: pinOf({provider: wanted.provider, model: entry?.id ?? wanted.model, label: wanted.label})}
}

// A pinned or legacy pair, checked against the loaded listing.
function resolveWanted(input: ModelSelectionInput, wanted: ModelTarget, reason: 'pinned' | 'legacy'): ModelSelection {
  const asIs: ModelSelection = {...wanted, label: shownLabel(wanted.label), reason}
  if (!input.listingLoaded) return asIs
  const provider = configuredProvider(input, wanted.provider)
  if (!provider) return unavailable(input, wanted, reason, undefined, {cause: 'provider'})
  const models = selectable(provider)
  // Nothing to chat with listed: no decision (see the header comment) — the
  // thread keeps its pair, with no notice and no pin.
  if (models.length === 0) return asIs
  const entry = findModel(wanted.model, models)
  if (entry) {
    if (!isUsable(entry)) {
      // A pair that never stored a name goes by its entry's.
      const named = storableModelLabel(wanted.label, wanted.model) ? wanted : {...wanted, label: labelOf(entry)}
      return unavailable(input, named, reason, entry, {
        cause: entry.status as UnavailableCause,
        statusReason: entry.statusReason,
      })
    }
    const target = {provider: provider.id, model: entry.id, label: labelOf(entry)}
    const pin = pinOf(target)
    const repin =
      reason === 'legacy' ||
      target.model !== wanted.model ||
      pin.modelLabel !== storableModelLabel(wanted.label, wanted.model)
    return repin ? {...target, reason, pin} : {...target, reason}
  }
  if (reason === 'pinned' && !COMPLETE_LISTING.has(provider.id)) return asIs
  if (reason === 'legacy' && servesUnlisted(input, provider, wanted.model)) {
    return {...asIs, pin: pinOf(wanted)}
  }
  return fallback(input, wanted, 'removed')
}

function legacyPair(messages: readonly SelectionMessage[] | undefined): ModelTarget | null {
  if (!messages) return null
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i]
    if (m?.role === 'assistant' && m.provider && m.model) {
      return {provider: m.provider, model: m.model, label: m.modelLabel || m.model}
    }
  }
  return null
}

/** The model a thread uses now, why, and what to pin or announce (see the header comment). */
export function resolveThreadModel(input: ModelSelectionInput): ModelSelection {
  const pin = input.pin
  if (pin?.provider && pin.model) {
    return resolveWanted(
      input,
      {provider: pin.provider, model: pin.model, label: pin.modelLabel || pin.model},
      'pinned',
    )
  }
  const legacy = legacyPair(input.messages)
  if (legacy) return resolveWanted(input, legacy, 'legacy')
  return newChatSelection(input)
}

export interface LegacyDefaultRewrite {
  /** The "m/" id to save as the provider's defaultModel instead (once). */
  rewriteTo: string | null
  /** The saved default names nothing any more: it is ignored, and a one-time notice says so. */
  unresolved: boolean
}

/**
 * The one-time rewrite of a saved Azure "Default for new chats" that is a
 * pre-registry id (`gpt-4.1`, `claude:x`, `agent:y` — they stop resolving
 * next release). One that the caller's OWN entry answers for through its
 * aliases becomes that entry's "m/" id. One a shared entry answers for is
 * left alone (the organisation's aliases can change under the member). One
 * nothing answers for is unresolved. Only decided once the listing loaded,
 * and never on a listing with nothing to chat with (the legacy import may
 * still be running); an "m/" id, or an id listed as is, needs nothing.
 */
export function legacyDefaultRewrite(
  saved: string | null | undefined,
  models: readonly SelectableModel[],
  listingLoaded: boolean,
): LegacyDefaultRewrite {
  const none: LegacyDefaultRewrite = {rewriteTo: null, unresolved: false}
  const id = saved?.trim() ?? ''
  if (!listingLoaded || !id || isRegistryModelId(id)) return none
  const listed = models.filter(m => m.kind !== 'embedding')
  if (listed.length === 0 || listed.some(m => m.id === id)) return none
  const own = listed.find(m => !m.shared && hasAlias(m, id))
  if (own) return {rewriteTo: own.id, unresolved: false}
  if (listed.some(m => hasAlias(m, id))) return none
  return {rewriteTo: null, unresolved: true}
}
