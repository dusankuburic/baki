import {useState, useEffect, useRef, useCallback, useMemo} from 'react'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {chatApi, providersApi} from '@/api'
import {logger} from '@/lib/logger'
import type {ProviderID, ModelDetail, ProviderInfo} from '@/types'

export interface ProviderOption {
  id: string
  name: string
  configured: boolean
  authType: string
  models: ModelDetail[]
  defaultModel: string
}

export function useProviderSetup() {
  const provider = useChatStore(s => s.selectedProvider)
  const setProvider = useChatStore(s => s.setProvider)
  const updateAI = useSettingsStore(s => s.updateAI)
  const aiSettings = useSettingsStore(s => s.settings.ai)

  const [configured, setConfigured] = useState(false)
  const [providers, setProviders] = useState<ProviderOption[]>([])
  const [selectedModel, setSelectedModel] = useState('')
  const [demoRemaining, setDemoRemaining] = useState<number | null>(null)

  const providerRef = useRef(provider)
  useEffect(() => { providerRef.current = provider })
  const aiSettingsProvidersRef = useRef(aiSettings.providers)
  useEffect(() => { aiSettingsProvidersRef.current = aiSettings.providers })

  useEffect(() => {
    let cancelled = false
    providersApi.listProviders().then((ps: ProviderInfo[]) => {
      if (cancelled) return
      const list: ProviderOption[] = (ps || []).map((p: ProviderInfo) => ({
        id: p.id || '',
        name: p.name || '',
        configured: !!p.configured,
        authType: p.authType || '',
        models: (p.models || []).map((m: ModelDetail) => ({
          id: m.id || '',
          displayName: m.displayName || m.id || '',
          contextLimit: m.contextLimit || 0,
          inputCostPerM: m.inputCostPerM || 0,
          outputCostPerM: m.outputCostPerM || 0,
        })),
        defaultModel: p.defaultModel || '',
      }))
      setProviders(list)

      const anyConfigured = list.some(p => p.configured)
      if (anyConfigured) {
        setConfigured(true)
        const cur = list.find(p => p.id === providerRef.current)
        if (!cur?.configured) {
          const first = list.find(p => p.configured)
          if (first) {
            setProvider(first.id as ProviderID)
            updateAI({activeProvider: first.id as ProviderID})
          }
        }
      } else {
        setConfigured(false)
      }
    }).catch((err) => { logger.warn('Failed to check provider status', err) })
    return () => { cancelled = true }
  }, [setProvider, updateAI])

  useEffect(() => {
    const config = aiSettingsProvidersRef.current[provider as keyof typeof aiSettings.providers]
    const prov = providers.find(p => p.id === provider)
    const model = config?.defaultModel || prov?.defaultModel || ''
    setSelectedModel(prev => prev === model ? prev : model)
  }, [provider, providers, aiSettings.providers])

  useEffect(() => {
    if (provider !== 'demo') {
      setDemoRemaining(null)
      return
    }
    let cancelled = false
    chatApi.getDemoRemaining()
      .then(r => { if (!cancelled) setDemoRemaining(r) })
      .catch(() => { if (!cancelled) setDemoRemaining(null) })
    return () => { cancelled = true }
  }, [provider])

  const handleSetProvider = useCallback((p: ProviderID) => {
    setProvider(p)
    updateAI({activeProvider: p})
  }, [setProvider, updateAI])

  const currentProvider = useMemo(() => providers.find(p => p.id === provider), [providers, provider])
  const currentModels = useMemo(() => currentProvider?.models ?? [], [currentProvider])
  const currentModelDetail = useMemo(
    () => currentModels.find(m => m.id === selectedModel),
    [currentModels, selectedModel],
  )

  return {
    configured,
    providers,
    selectedModel,
    setSelectedModel,
    demoRemaining,
    handleSetProvider,
    currentProvider,
    currentModels,
    currentModelDetail,
  }
}
