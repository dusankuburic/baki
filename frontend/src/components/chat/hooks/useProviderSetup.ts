import {useState, useEffect, useRef, useCallback, useMemo} from 'react'
import {useChatStore, isProviderId, type ModelRefusal} from '@/stores/chatStore'
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
  /** Credentials resolved from the active organisation, not the user. */
  shared?: boolean
  /**
   * Azure AI Foundry: the model picker's "Your project has models you
   * haven't added · Review" line, for users whose legacy Entra + project
   * resource used to list its deployments and agents automatically.
   */
  suggestDiscovery?: boolean
}

/**
 * A notice the model row shows: the thread's model was removed or is
 * unavailable (lib/modelSelection), or the server refused a send for it
 * while this scope's listing still offers it ('refused' — see
 * chatStore.ModelRefusal).
 */
export type ThreadModelNotice = ModelNoticeData | {kind: 'refused'; wanted: ModelTarget; fallback: null}

/** The one-time notice for a saved Azure "Default for new chats" that no longer names anything. */
export interface LegacyDefaultNotice {
  saved: string
  /** What new Azure chats start on instead; null when nothing is usable. */
  fallback: string | null
}

const EMPTY_PROVIDERS: ProviderOption[] = []
const EMPTY_MODELS: ModelDetail[] = []

const MODEL_KINDS: ReadonlySet<string> = new Set(['chat', 'claude', 'agent', 'embedding'])

// toModelDetail copies a listed model field by field — the listing is
// untrusted input, and a field left out here never reaches the picker (the
// registry fields were once dropped exactly that way). An unknown status is
// kept: a newer server's status for an entry it cannot serve must not read
// as usable.
function toModelDetail(m: ModelDetail): ModelDetail {
  const out: ModelDetail = {
    id: m.id || '',
    displayName: m.displayName || m.id || '',
    contextLimit: m.contextLimit || 0,
    inputCostPerM: m.inputCostPerM || 0,
    outputCostPerM: m.outputCostPerM || 0,
  }
  // Drives the header's "no native tools" hint; dropping it here meant the
  // hint never showed.
  if (typeof m.supportsTools === 'boolean') out.supportsTools = m.supportsTools
  if (typeof m.kind === 'string' && MODEL_KINDS.has(m.kind)) out.kind = m.kind
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
  }
}

// The saved Azure default that was acknowledged as unresolved (its notice
// dismissed) is remembered per user, so the notice shows once per value.
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
    // Storage unavailable: the notice then returns next session.
  }
}

// The listing as loaded: which org scope ('' = none) and provider epoch it
// was fetched for.
interface Listing {
  scope: string
  epoch: number
  providers: ProviderOption[]
}

// useProviderSetup is the chat panel's view of the AI providers: the listing
// for the open flow's scope, and — per-thread model memory — the ACTIVE
// thread's provider/model, resolved by lib/modelSelection from the thread's
// pin, its history (a thread from before the upgrade) and the active
// provider's "Default for new chats".
//
// Nothing here writes settings on a fallback: a thread whose model cannot be
// used falls back for itself only (with a notice), and `activeProvider`
// stays the provider for new chats. A header pick pins the active thread and
// nothing else. The one settings write left is the one-time rewrite of a
// saved pre-registry Azure default onto its "m/" entry.
export function useProviderSetup() {
  const providerEpoch = useChatStore(s => s.providerEpoch)
  const updateProvider = useSettingsStore(s => s.updateProvider)
  const aiSettings = useSettingsStore(s => s.settings.ai)
  // An organisation's shared credentials count as configured for its members
  // (the user's own always win), so the list is scoped to an org and
  // refetched when that changes. Which org: a chat turn resolves shared
  // credentials through the OPEN flow's organisation, so while a flow is open
  // the list follows it — a personal flow lists no shared provider even under
  // an active org, and an org-B flow never shows org A's deployments. The
  // active org only applies when no flow is open.
  const activeOrgId = useOrgStore(s => s.activeOrgId)
  const flowOpen = useFlowStore(s => s.document !== null)
  const flowOrgId = useFlowStore(s => s.document?.orgId ?? '')
  const listOrgId = (flowOpen ? flowOrgId : (activeOrgId ?? '')) || undefined
  const scope = listOrgId ?? ''
  const orgName = useOrgStore(s => (listOrgId ? s.organisations.find(o => o.id === listOrgId)?.name : undefined))

  // The active thread: its pin, and its messages (only an unpinned thread's
  // are read — its last reply names the model it used before the upgrade).
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
  // Whether the thread's server history is in: until then an unpinned
  // thread's messages are not known, so it cannot be told from a new one.
  const historySynced = useChatStore(s =>
    s.activeThreadId ? s.historyStatus[s.activeThreadId]?.state === 'loaded' : false,
  )

  // null = still checking. Starting at false flashed "add an API key" on
  // every panel mount, and a failed check left that screen up for good.
  const [configured, setConfigured] = useState<boolean | null>(null)
  // The provider epoch whose fetch failed last; null once a fetch lands (or
  // a retry starts). A refusal the server made is shown even when the
  // refetch it asked for fails.
  const [failedEpoch, setFailedEpoch] = useState<number | null>(null)
  const providersError = failedEpoch !== null
  const [listing, setListing] = useState<Listing | null>(null)

  // The epoch the last fetch ran for. A bump means "the listing changed"
  // (a key saved, a model added or removed, a deployment's served model
  // revealed by the first turn), so that refetch bypasses the 5 s GET
  // micro-cache — it would otherwise hand back the listing from before the
  // change. The first fetch and a scope change may share a cached round trip
  // with sibling panels.
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
  // Loaded = a listing for THIS scope arrived and the last fetch did not
  // fail. Only then may a model count as removed or unavailable: while the
  // listing loads (or after it failed) every thread keeps its own pair and
  // no notice shows.
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

  // A selection that says so writes its pin to the thread, once: a legacy
  // thread's inferred pair, a pinned pre-registry id moved onto its "m/"
  // entry, a renamed entry's new label. The write changes the pin, the
  // selection then needs nothing more.
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

  // The server refused a send for the thread's model, and the listing
  // refetched since still offers it (normally it no longer does, and the
  // selection's own removal notice says so) — or that refetch failed: the
  // refusal stands on its own then, the server having just made it.
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

  // The pair a turn on the active thread goes to. While the thread's own
  // model is only temporarily unavailable, a turn uses the stand-in without
  // re-pinning (the thread returns to its model once it can be used again).
  // Nor does a turn pin an unpinned thread whose history is still loading:
  // it may turn out to be a thread from before per-thread memory, whose
  // model legacy inference reads off that history. Its label is only ever
  // a name — never the id (see storableModelLabel).
  const pinned = !!pinProvider && !!pinModel
  const turnModel: TurnModel = useMemo(() => {
    const modelLabel = storableModelLabel(selection.label, selection.model)
    return {
      provider: isProviderId(selection.provider) ? selection.provider : aiSettings.activeProvider,
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

  // The model each configured provider's new chats start on in this scope:
  // the picker's [DEFAULT] badges.
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

  // A header pick pins the ACTIVE thread — only it — and settles any refusal
  // on it. No settings write: "Default for new chats" is AI Accounts' and AI
  // Behavior's to change.
  const pickModel = useCallback((providerId: string, model: Pick<ModelDetail, 'id' | 'displayName'>) => {
    const st = useChatStore.getState()
    const threadId = st.activeThreadId
    if (!threadId) return
    st.setThreadModel(threadId, {provider: providerId, model: model.id, modelLabel: model.displayName || model.id})
    st.setModelRefusal(threadId, null)
  }, [])

  // Dismissing a removal notice pins the thread to its fallback (the model it
  // named is gone for good); a refusal notice just goes away.
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

  // ---- The saved Azure "Default for new chats" from before the registry ----
  //
  // A pre-registry id (`gpt-4.1`, `claude:x`, `agent:y`) stops resolving next
  // release. One the caller's own entry answers for is rewritten to its "m/"
  // id, once; one nothing answers for is ignored (new chats use the server
  // default) and a notice says so until it is dismissed.
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
    // Once per value per session: a failed write rolls the field back and
    // must not be retried in a loop.
    const key = `${savedAzureDefault}\u0000${to}`
    if (rewrittenRef.current.has(key)) return
    rewrittenRef.current.add(key)
    void updateProvider('azure-foundry', {defaultModel: to})
  }, [legacyDefault.rewriteTo, savedAzureDefault, updateProvider])

  const [dismissedLegacyDefault, setDismissedLegacyDefault] = useState<string | null>(readDismissedLegacyDefault)
  const legacyDefaultNotice: LegacyDefaultNotice | null = useMemo(() => {
    // Only where it matters: new chats start on Azure.
    if (!legacyDefault.unresolved || !azure || aiSettings.activeProvider !== 'azure-foundry') return null
    if (dismissedLegacyDefault === savedAzureDefault) return null
    return {saved: savedAzureDefault, fallback: providerNewChatDefault(settings, azure)?.label || null}
  }, [legacyDefault.unresolved, azure, aiSettings.activeProvider, dismissedLegacyDefault, savedAzureDefault, settings])
  const dismissLegacyDefaultNotice = useCallback(() => {
    writeDismissedLegacyDefault(savedAzureDefault)
    setDismissedLegacyDefault(savedAzureDefault)
  }, [savedAzureDefault])

  // Retry a failed provider check (the list refetches on an epoch bump).
  const retryProviders = useCallback(() => {
    // Back to "checking" so the panel shows progress instead of the stale
    // error while the list refetches.
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
    /** The active thread's model: provider, model ('' = nothing usable), label and why. */
    selection,
    notice,
    dismissNotice,
    turnModel,
    pickModel,
    threadProvider,
    threadModels,
    threadModelDetail,
    newChatDefaults,
    /** The listing scope's organisation name (for "Shared by …"). */
    orgName,
    legacyDefaultNotice,
    dismissLegacyDefaultNotice,
    /** What selection reads from settings (other threads resolve with it too: thread-tab tooltips). */
    settings,
  }
}
