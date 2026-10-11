import {useState, useEffect, useRef, useCallback, useMemo} from 'react'
import {useChatStore, type ModelRefusal} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useOrgStore} from '@/stores/orgStore'
import {useFlowStore} from '@/stores/flowStore'
import {currentUserId} from '@/stores/storeRegistry'
import {providersApi} from '@/api'
import {logger} from '@/lib/logger'
import {findModel, storableModelLabel} from '@/lib/modelIds'
import {
  legacyDefaultRewrite,
  providerNewChatDefault,
  resolveThreadModel,
  type ModelNoticeData,
  type ModelSelection,
  type ModelTarget,
  type SelectionSettings,
} from '@/lib/modelSelection'
import type {TurnModel} from './useChatStreamEngine'
import type {ChatMessage, ModelDetail, ProviderInfo} from '@/types'

export interface ProviderOption {
  id: string
  name: string
  configured: boolean
  authType: string
  models: ModelDetail[]
  defaultModel: string
  shared?: boolean
  suggestDiscovery?: boolean
  icon?: string
}

export type ThreadModelNotice = ModelNoticeData | {kind: 'refused'; wanted: ModelTarget; fallback: null}

export interface LegacyDefaultNotice {
  saved: string
  fallback: string | null
}

const EMPTY_PROVIDERS: ProviderOption[] = []
const EMPTY_MODELS: ModelDetail[] = []

function toModelDetail(m: ModelDetail): ModelDetail {
  const out: ModelDetail = {
    id: m.id || '',
    displayName: m.displayName || m.id || '',
    contextLimit: m.contextLimit || 0,
    inputCostPerM: m.inputCostPerM || 0,
    outputCostPerM: m.outputCostPerM || 0,
  }
  if (typeof m.supportsTools === 'boolean') out.supportsTools = m.supportsTools
  if (typeof m.kind === 'string' && m.kind) out.kind = m.kind
  if (typeof m.purpose === 'string' && m.purpose) out.purpose = m.purpose
  if (typeof m.purposeCustom === 'string' && m.purposeCustom) out.purposeCustom = m.purposeCustom
  if (typeof m.description === 'string' && m.description) out.description = m.description
  if (m.shared === true) out.shared = true
  if (typeof m.connectionId === 'string' && m.connectionId) out.connectionId = m.connectionId
  if (typeof m.connectionName === 'string' && m.connectionName) out.connectionName = m.connectionName
  if (typeof m.status === 'string' && m.status) out.status = m.status
  if (typeof m.statusReason === 'string' && m.statusReason) out.statusReason = m.statusReason
  if (Array.isArray(m.aliases)) {
    const aliases = m.aliases.filter((a): a is string => typeof a === 'string' && a !== '')
    if (aliases.length > 0) out.aliases = aliases
  }
  if (m.isDefault === true) out.isDefault = true
  if (m.isOrgDefault === true) out.isOrgDefault = true
  return out
}

function toProviderOption(p: ProviderInfo): ProviderOption {
  return {
    id: p.id || '',
    name: p.name || '',
    configured: !!p.configured,
    authType: p.authType || '',
    ...(p.shared ? {shared: true} : {}),
    models: (p.models || []).map(toModelDetail),
    defaultModel: p.defaultModel || '',
    ...(p.suggestDiscovery === true ? {suggestDiscovery: true} : {}),
    ...(p.icon ? {icon: p.icon} : {}),
  }
}

const LEGACY_NOTICE_KEY = 'baki:chat:legacyDefaultNotice:v1'

function legacyNoticeKey(): string {
  return `${LEGACY_NOTICE_KEY}:${currentUserId() ?? 'local'}`
}

function readDismissedLegacyDefault(): string | null {
  try {
    return localStorage.getItem(legacyNoticeKey())
  } catch {
    return null
  }
}

function writeDismissedLegacyDefault(value: string): void {
  try {
    localStorage.setItem(legacyNoticeKey(), value)
  } catch {
  }
}

interface Listing {
  scope: string
  epoch: number
  providers: ProviderOption[]
}

export function useProviderSetup() {
  const providerEpoch = useChatStore(s => s.providerEpoch)
  const updateProvider = useSettingsStore(s => s.updateProvider)
  const aiSettings = useSettingsStore(s => s.settings.ai)
  const activeOrgId = useOrgStore(s => s.activeOrgId)
  const flowOpen = useFlowStore(s => s.document !== null)
  const flowOrgId = useFlowStore(s => s.document?.orgId ?? '')
  const listOrgId = (flowOpen ? flowOrgId : (activeOrgId ?? '')) || undefined
  const scope = listOrgId ?? ''
  const orgName = useOrgStore(s => (listOrgId ? s.organisations.find(o => o.id === listOrgId)?.name : undefined))

  const activeThreadId = useChatStore(s => s.activeThreadId)
  const pinProvider = useChatStore(s => s.threads.find(t => t.id === s.activeThreadId)?.provider)
  const pinModel = useChatStore(s => s.threads.find(t => t.id === s.activeThreadId)?.model)
  const pinLabel = useChatStore(s => s.threads.find(t => t.id === s.activeThreadId)?.modelLabel)
  const threadMessages: readonly ChatMessage[] | undefined = useChatStore(s =>
    s.activeThreadId ? s.conversations.get(s.activeThreadId) : undefined,
  )
  const refusal: ModelRefusal | undefined = useChatStore(s =>
    s.activeThreadId ? s.modelRefusals[s.activeThreadId] : undefined,
  )
  const historySynced = useChatStore(s =>
    s.activeThreadId ? s.historyStatus[s.activeThreadId]?.state === 'loaded' : false,
  )

  const [configured, setConfigured] = useState<boolean | null>(null)
  const [failedEpoch, setFailedEpoch] = useState<number | null>(null)
  const providersError = failedEpoch !== null
  const [listing, setListing] = useState<Listing | null>(null)

  const fetchedEpochRef = useRef(providerEpoch)

  useEffect(() => {
    let cancelled = false
    const bypassCache = fetchedEpochRef.current !== providerEpoch
    fetchedEpochRef.current = providerEpoch
    const epoch = providerEpoch
    providersApi
      .listProviders(listOrgId, bypassCache ? {noDedup: true} : undefined)
      .then((ps: ProviderInfo[]) => {
        if (cancelled) return
        const list = (ps || []).map(toProviderOption)
        setListing({scope: listOrgId ?? '', epoch, providers: list})
        setFailedEpoch(null)
        setConfigured(list.some(p => p.configured))
      })
      .catch(err => {
        if (cancelled) return
        logger.warn('Failed to check provider status', err)
        setFailedEpoch(epoch)
      })
    return () => {
      cancelled = true
    }
  }, [providerEpoch, listOrgId])

  const providers = listing?.providers ?? EMPTY_PROVIDERS
  const listingLoaded = listing !== null && listing.scope === scope && !providersError

  const settings: SelectionSettings = useMemo(
    () => ({activeProvider: aiSettings.activeProvider, providers: aiSettings.providers}),
    [aiSettings.activeProvider, aiSettings.providers],
  )

  const selection: ModelSelection = useMemo(
    () =>
      resolveThreadModel({
        pin: pinProvider && pinModel ? {provider: pinProvider, model: pinModel, modelLabel: pinLabel} : null,
        messages: threadMessages,
        settings,
        providers,
        listingLoaded,
      }),
    [pinProvider, pinModel, pinLabel, threadMessages, settings, providers, listingLoaded],
  )

  const selectionPin = selection.pin
  useEffect(() => {
    if (!activeThreadId || !selectionPin) return
    useChatStore.getState().setThreadModel(activeThreadId, selectionPin)
  }, [activeThreadId, selectionPin])

  const threadProvider = useMemo(
    () => providers.find(p => p.id === selection.provider),
    [providers, selection.provider],
  )
  const threadModels = threadProvider?.models ?? EMPTY_MODELS
  const threadModelDetail = useMemo(
    () => (selection.model ? findModel(selection.model, threadModels) : undefined),
    [selection.model, threadModels],
  )

  const notice: ThreadModelNotice | null = useMemo(() => {
    if (selection.notice) return selection.notice
    if (!refusal) return null
    const refetched = !!listing && listingLoaded && listing.epoch >= refusal.epoch
    const refetchFailed = failedEpoch !== null && failedEpoch >= refusal.epoch
    if (!refetched && !refetchFailed) return null
    if (refusal.provider !== selection.provider || refusal.model !== selection.model) return null
    return {
      kind: 'refused',
      wanted: {provider: refusal.provider, model: refusal.model, label: refusal.modelLabel || selection.label},
      fallback: null,
    }
  }, [selection, refusal, listing, listingLoaded, failedEpoch])

  const pinned = !!pinProvider && !!pinModel
  const turnModel: TurnModel = useMemo(() => {
    const modelLabel = storableModelLabel(selection.label, selection.model)
    return {
      provider: selection.provider || aiSettings.activeProvider,
      model: selection.model,
      ...(modelLabel ? {modelLabel} : {}),
      pinThread: selection.reason !== 'fallbackUnavailable' && (pinned || historySynced),
    }
  }, [
    selection.provider,
    selection.model,
    selection.label,
    selection.reason,
    aiSettings.activeProvider,
    pinned,
    historySynced,
  ])

  const newChatDefaults = useMemo(() => {
    const out: Record<string, string> = {}
    if (!listingLoaded) return out
    for (const p of providers) {
      if (!p.configured) continue
      const target = providerNewChatDefault(settings, p)
      if (target?.model) out[p.id] = target.model
    }
    return out
  }, [providers, settings, listingLoaded])

  const pickModel = useCallback((providerId: string, model: Pick<ModelDetail, 'id' | 'displayName'>) => {
    const st = useChatStore.getState()
    const threadId = st.activeThreadId
    if (!threadId) return
    st.setThreadModel(threadId, {provider: providerId, model: model.id, modelLabel: model.displayName || model.id})
    st.setModelRefusal(threadId, null)
  }, [])

  const dismissNotice = useCallback(() => {
    const st = useChatStore.getState()
    const threadId = st.activeThreadId
    if (!threadId || !notice) return
    if (notice.kind === 'removed' && notice.fallback) {
      st.setThreadModel(threadId, {
        provider: notice.fallback.provider,
        model: notice.fallback.model,
        modelLabel: notice.fallback.label,
      })
    }
    st.setModelRefusal(threadId, null)
  }, [notice])

  const azure = providers.find(p => p.id === 'azure-foundry')
  const azureModels = azure?.models ?? EMPTY_MODELS
  const savedAzureDefault = aiSettings.providers['azure-foundry']?.defaultModel?.trim() ?? ''
  const azureListed = listingLoaded && !!azure?.configured
  const legacyDefault = useMemo(
    () => legacyDefaultRewrite(savedAzureDefault, azureModels, azureListed),
    [savedAzureDefault, azureModels, azureListed],
  )
  const rewrittenRef = useRef(new Set<string>())
  useEffect(() => {
    const to = legacyDefault.rewriteTo
    if (!to) return
    const key = `${savedAzureDefault}\u0000${to}`
    if (rewrittenRef.current.has(key)) return
    rewrittenRef.current.add(key)
    void updateProvider('azure-foundry', {defaultModel: to})
  }, [legacyDefault.rewriteTo, savedAzureDefault, updateProvider])

  const [dismissedLegacyDefault, setDismissedLegacyDefault] = useState<string | null>(readDismissedLegacyDefault)
  const legacyDefaultNotice: LegacyDefaultNotice | null = useMemo(() => {
    if (!legacyDefault.unresolved || !azure || aiSettings.activeProvider !== 'azure-foundry') return null
    if (dismissedLegacyDefault === savedAzureDefault) return null
    return {saved: savedAzureDefault, fallback: providerNewChatDefault(settings, azure)?.label || null}
  }, [legacyDefault.unresolved, azure, aiSettings.activeProvider, dismissedLegacyDefault, savedAzureDefault, settings])
  const dismissLegacyDefaultNotice = useCallback(() => {
    writeDismissedLegacyDefault(savedAzureDefault)
    setDismissedLegacyDefault(savedAzureDefault)
  }, [savedAzureDefault])

  const retryProviders = useCallback(() => {
    setFailedEpoch(null)
    setConfigured(null)
    useChatStore.getState().bumpProviderEpoch()
  }, [])

  return {
    configured,
    providersError,
    retryProviders,
    providers,
    listingLoaded,
    selection,
    notice,
    dismissNotice,
    turnModel,
    pickModel,
    threadProvider,
    threadModels,
    threadModelDetail,
    newChatDefaults,
    orgName,
    legacyDefaultNotice,
    dismissLegacyDefaultNotice,
    settings,
  }
}
