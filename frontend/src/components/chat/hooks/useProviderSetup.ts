import {useState, useEffect, useRef, useCallback, useMemo} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useOrgStore} from '@/stores/orgStore'
import {useFlowStore} from '@/stores/flowStore'
import {providersApi} from '@/api'
import {logger} from '@/lib/logger'
import type {AISettings, ProviderID, ModelDetail, ProviderInfo} from '@/types'

export interface ProviderOption {
  id: string
  name: string
  configured: boolean
  authType: string
  models: ModelDetail[]
  defaultModel: string
  /** Credentials resolved from the active organisation, not the user. */
  shared?: boolean
}

export function useProviderSetup() {
  const provider = useChatStore(s => s.selectedProvider)
  const setProvider = useChatStore(s => s.setProvider)
  const providerEpoch = useChatStore(s => s.providerEpoch)
  const updateAI = useSettingsStore(s => s.updateAI)
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

  // null = still checking. Starting at false flashed "add an API key" on
  // every panel mount, and a failed check left that screen up for good.
  const [configured, setConfigured] = useState<boolean | null>(null)
  const [providersError, setProvidersError] = useState(false)
  const [providers, setProviders] = useState<ProviderOption[]>([])
  const [selectedModel, setSelectedModelState] = useState('')
  // userPickedRef: the header selection came from the user (not a default),
  // so settings reloads and provider-list refetches must keep it.
  const userPickedRef = useRef(false)

  // A provider change from anywhere — the header or Settings' Active
  // Assistant — starts on that provider's default model: a model id shared
  // across providers must not carry over, and the pick flag must not pin it.
  const lastProviderRef = useRef(provider)
  useEffect(() => {
    if (lastProviderRef.current === provider) return
    lastProviderRef.current = provider
    userPickedRef.current = false
  }, [provider])
  const aiSettingsProvidersRef = useRef(aiSettings.providers)
  useEffect(() => {
    aiSettingsProvidersRef.current = aiSettings.providers
  })

  // listScopeRef: the org scope ('' = none) of the list currently in
  // `providers`, set together with it. fallbackScopeRef: the scope the
  // fallback effect last acted on — a differing one means the list changed
  // because the scope did (see that effect).
  const listScopeRef = useRef<string | null>(null)
  const fallbackScopeRef = useRef<string | null>(null)
  // The epoch the last fetch ran for. A bump means "the listing changed"
  // (a key saved, a deployment's served model revealed by the first turn),
  // so that refetch bypasses the 5 s GET micro-cache — it would otherwise
  // hand back the listing from before the change. The first fetch and a
  // scope change may share a cached round trip with sibling panels.
  const fetchedEpochRef = useRef(providerEpoch)

  useEffect(() => {
    let cancelled = false
    const bypassCache = fetchedEpochRef.current !== providerEpoch
    fetchedEpochRef.current = providerEpoch
    const scope = listOrgId ?? ''
    providersApi
      .listProviders(listOrgId, bypassCache ? {noDedup: true} : undefined)
      .then((ps: ProviderInfo[]) => {
        if (cancelled) return
        listScopeRef.current = scope
        const list: ProviderOption[] = (ps || []).map((p: ProviderInfo) => ({
          id: p.id || '',
          name: p.name || '',
          configured: !!p.configured,
          authType: p.authType || '',
          ...(p.shared ? {shared: true} : {}),
          models: (p.models || []).map((m: ModelDetail) => ({
            id: m.id || '',
            displayName: m.displayName || m.id || '',
            contextLimit: m.contextLimit || 0,
            inputCostPerM: m.inputCostPerM || 0,
            outputCostPerM: m.outputCostPerM || 0,
            // Drives the header's "no native tools" hint; dropping it here
            // meant the hint never showed.
            ...(typeof m.supportsTools === 'boolean' ? {supportsTools: m.supportsTools} : {}),
          })),
          defaultModel: p.defaultModel || '',
        }))
        setProviders(list)
        setProvidersError(false)

        setConfigured(list.some(p => p.configured))
      })
      .catch(err => {
        if (cancelled) return
        logger.warn('Failed to check provider status', err)
        setProvidersError(true)
      })
    return () => {
      cancelled = true
    }
  }, [providerEpoch, listOrgId])

  // The chat's provider must be one with a key. Re-applied on every provider
  // change, not only when the list loads: Settings' Active Assistant syncs the
  // chat provider too, and an unconfigured pick left the header on "Select
  // provider" with every send erroring. The fallback is written back so
  // Settings shows the provider the chat actually uses — unless the list
  // changed because its org scope did (another flow or org opened): the
  // provider is only unavailable HERE, so the session switches but the
  // user's saved choice stays for the scopes that do offer it.
  useEffect(() => {
    if (providers.length === 0) return
    const scope = listScopeRef.current
    const scopeChanged = fallbackScopeRef.current !== null && fallbackScopeRef.current !== scope
    fallbackScopeRef.current = scope
    if (providers.find(p => p.id === provider)?.configured) return
    const first = providers.find(p => p.configured)
    if (!first || first.id === provider) return
    setProvider(first.id as ProviderID)
    if (!scopeChanged) void updateAI({activeProvider: first.id as ProviderID})
  }, [provider, providers, setProvider, updateAI])

  useEffect(() => {
    // Keyed off the TYPE, not the live `aiSettings` value: referencing the
    // value here (even only in a type position) made exhaustive-deps demand
    // `aiSettings` as a dependency, which would re-run this on every
    // unrelated settings write.
    const config = aiSettingsProvidersRef.current[provider as keyof AISettings['providers']]
    const prov = providers.find(p => p.id === provider)
    // An Azure AI Foundry default saved before the listing named exactly the
    // configured deployments can name one that no longer exists; the turn
    // would fail with DeploymentNotFound. Once the listing has loaded, such a
    // saved default gives way to the provider's own.
    const staleAzureDefault =
      provider === 'azure-foundry' &&
      !!config?.defaultModel &&
      !!prov &&
      prov.models.length > 0 &&
      !prov.models.some(m => m.id === config.defaultModel)
    const model = (staleAzureDefault ? '' : config?.defaultModel) || prov?.defaultModel || ''
    // This re-runs on every settings reload — settings:changed also fires for
    // this tab's OWN layout saves — and on provider-list refetches. Resetting
    // to the default each time silently switched the user's model back a few
    // seconds after they collapsed a pane. A user pick stays while the
    // provider still offers it; otherwise the default applies.
    setSelectedModelState(prev => {
      if (userPickedRef.current && prev && prov?.models.some(m => m.id === prev)) return prev
      return prev === model ? prev : model
    })
  }, [provider, providers, aiSettings.providers])

  const setSelectedModel = useCallback((model: string) => {
    userPickedRef.current = true
    setSelectedModelState(model)
  }, [])

  const handleSetProvider = useCallback(
    (p: ProviderID) => {
      // A new provider starts on its default model.
      userPickedRef.current = false
      setProvider(p)
      void updateAI({activeProvider: p})
    },
    [setProvider, updateAI],
  )

  // Retry a failed provider check (the list refetches on an epoch bump).
  const retryProviders = useCallback(() => {
    // Back to "checking" so the panel shows progress instead of the stale
    // error while the list refetches.
    setProvidersError(false)
    setConfigured(null)
    useChatStore.getState().bumpProviderEpoch()
  }, [])

  const currentProvider = useMemo(() => providers.find(p => p.id === provider), [providers, provider])
  const currentModels = useMemo(() => currentProvider?.models ?? [], [currentProvider])
  const currentModelDetail = useMemo(
    () => currentModels.find(m => m.id === selectedModel),
    [currentModels, selectedModel],
  )

  return {
    configured,
    providersError,
    retryProviders,
    providers,
    selectedModel,
    setSelectedModel,
    handleSetProvider,
    currentProvider,
    currentModels,
    currentModelDetail,
  }
}
