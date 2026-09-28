import {useState, useEffect, useRef, useCallback, useMemo} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useOrgStore} from '@/stores/orgStore'
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
  // The active organisation's shared credentials count as configured for
  // its members (the user's own always win), so the list is scoped to it
  // and refetched when it changes.
  const activeOrgId = useOrgStore(s => s.activeOrgId)

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

  useEffect(() => {
    let cancelled = false
    providersApi
      .listProviders(activeOrgId ?? undefined)
      .then((ps: ProviderInfo[]) => {
        if (cancelled) return
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
  }, [providerEpoch, activeOrgId])

  // The chat's provider must be one with a key. Re-applied on every provider
  // change, not only when the list loads: Settings' Active Assistant syncs the
  // chat provider too, and an unconfigured pick left the header on "Select
  // provider" with every send erroring. The fallback is written back so
  // Settings shows the provider the chat actually uses.
  useEffect(() => {
    if (providers.length === 0) return
    if (providers.find(p => p.id === provider)?.configured) return
    const first = providers.find(p => p.configured)
    if (!first || first.id === provider) return
    setProvider(first.id as ProviderID)
    void updateAI({activeProvider: first.id as ProviderID})
  }, [provider, providers, setProvider, updateAI])

  useEffect(() => {
    // Keyed off the TYPE, not the live `aiSettings` value: referencing the
    // value here (even only in a type position) made exhaustive-deps demand
    // `aiSettings` as a dependency, which would re-run this on every
    // unrelated settings write.
    const config = aiSettingsProvidersRef.current[provider as keyof AISettings['providers']]
    const prov = providers.find(p => p.id === provider)
    const model = config?.defaultModel || prov?.defaultModel || ''
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
