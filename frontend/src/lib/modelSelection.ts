import type {AzureStatus, Tolerant} from '@/types'
import {isPickerModel} from './modelGroups'
import {findModel, hasAlias, isRegistryModelId, storableModelLabel} from './modelIds'

const COMPLETE_LISTING = new Set(['azure-foundry', 'local'])

export interface SelectableModel {
  id: string
  displayName?: string
  kind?: string
  status?: Tolerant<AzureStatus>
  statusReason?: string
  aliases?: readonly string[] | null
  shared?: boolean
}

export interface SelectableProvider {
  id: string
  configured: boolean
  defaultModel: string
  models: readonly SelectableModel[]
}

export interface ThreadModelPin {
  provider: string
  model: string
  modelLabel?: string
}

export interface SelectionMessage {
  role: string
  provider?: string
  model?: string
  modelLabel?: string
}

export interface SelectionSettings {
  activeProvider: string
  providers: Partial<Record<string, {defaultModel?: string} | undefined>>
}

export interface ModelSelectionInput {
  pin?: Partial<ThreadModelPin> | null
  messages?: readonly SelectionMessage[]
  settings: SelectionSettings
  providers: readonly SelectableProvider[]
  listingLoaded: boolean
}

export type ModelSelectionReason =
  'pinned' | 'default' | 'serverDefault' | 'legacy' | 'fallbackRemoved' | 'fallbackUnavailable'

export interface ModelTarget {
  provider: string
  model: string
  label: string
}

export type UnavailableCause = Exclude<Tolerant<AzureStatus>, 'ready'> | 'provider'

export interface ModelNoticeData {
  kind: 'removed' | 'unavailable'
  wanted: ModelTarget
  fallback: ModelTarget | null
  cause?: UnavailableCause
  statusReason?: string
}

export interface ModelSelection extends ModelTarget {
  reason: ModelSelectionReason
  pin?: ThreadModelPin
  notice?: ModelNoticeData
}

type DefaultTarget = ModelTarget & {reason: 'default' | 'serverDefault'}

function isUsable(model: SelectableModel): boolean {
  return !model.status || model.status === 'ready'
}

function selectable(provider: SelectableProvider): SelectableModel[] {
  return provider.models.filter(isPickerModel)
}

function labelOf(model: SelectableModel): string {
  return model.displayName || model.id
}

function shownLabel(label: string): string {
  return isRegistryModelId(label) ? '' : label
}

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

function servesUnlisted(input: ModelSelectionInput, provider: SelectableProvider, model: string): boolean {
  if (COMPLETE_LISTING.has(provider.id)) return false
  return model === savedDefault(input, provider.id) || model === provider.defaultModel.trim()
}

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

export function providerNewChatDefault(
  settings: SelectionSettings,
  provider: SelectableProvider,
): (ModelTarget & {reason: 'default' | 'serverDefault'}) | null {
  return providerDefault({settings, providers: [provider], listingLoaded: true}, provider)
}

function newChatSelection(input: ModelSelectionInput): ModelSelection {
  const {activeProvider} = input.settings
  if (!input.listingLoaded) {
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

function resolveWanted(input: ModelSelectionInput, wanted: ModelTarget, reason: 'pinned' | 'legacy'): ModelSelection {
  const asIs: ModelSelection = {...wanted, label: shownLabel(wanted.label), reason}
  if (!input.listingLoaded) return asIs
  const provider = configuredProvider(input, wanted.provider)
  if (!provider) return unavailable(input, wanted, reason, undefined, {cause: 'provider'})
  const models = selectable(provider)
  if (models.length === 0) return asIs
  const entry = findModel(wanted.model, models)
  if (entry) {
    if (!isUsable(entry)) {
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
  rewriteTo: string | null
  unresolved: boolean
}

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
