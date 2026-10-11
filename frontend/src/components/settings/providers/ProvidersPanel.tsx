import {useCallback, useEffect, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {ErrorState} from '@/components/shared'
import {providersApi} from '@/api'
import {ApiError} from '@/api/client'
import {getErrorMessage} from '@/lib/getErrorMessage'
import {logger} from '@/lib/logger'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useSystemStore} from '@/stores/systemStore'
import type {ProviderSpec} from '@/types'
import LegacyProvidersPanel from './LegacyProvidersPanel'
import ProviderSection from './ProviderSection'

type CatalogState =
  | {kind: 'loading'}
  | {kind: 'ready'; specs: ProviderSpec[]}
  | {kind: 'legacy'}
  | {kind: 'error'; message: string}

export default function ProvidersPanel() {
  const {t} = useTranslation('settings')
  const [state, setState] = useState<CatalogState>({kind: 'loading'})
  const [hidden, setHidden] = useState<string[]>([])
  const alive = useRef(true)
  const bumpProviderEpoch = useChatStore(s => s.bumpProviderEpoch)
  const setProvider = useChatStore(s => s.setProvider)
  const updateAI = useSettingsStore(s => s.updateAI)
  const knownRemoteBackend = useSystemStore(s => (s.info ? !s.info.capabilities.sessionAnalytics : false))

  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  const fetchCatalog = useCallback(() => {
    providersApi
      .getProviderCatalog({noDedup: true})
      .then(specs => {
        if (alive.current) setState({kind: 'ready', specs})
      })
      .catch((err: unknown) => {
        if (!alive.current) return
        if (err instanceof ApiError && err.status === 404) {
          setState({kind: 'legacy'})
          return
        }
        logger.warn('Failed to load the provider catalogue', err)
        setState({kind: 'error', message: getErrorMessage(err, t('providers.catalogLoadFailed'))})
      })
  }, [t])

  useEffect(() => {
    fetchCatalog()
  }, [fetchCatalog])

  const retry = useCallback(() => {
    setState({kind: 'loading'})
    fetchCatalog()
  }, [fetchCatalog])

  const onConfigured = useCallback(() => bumpProviderEpoch(), [bumpProviderEpoch])

  const onActivated = useCallback(
    async (id: string, when: 'always' | 'ifUnconfigured') => {
      if (when === 'ifUnconfigured') {
        const current = useSettingsStore.getState().settings.ai.activeProvider
        if (current === id) return
        let currentConfigured: boolean
        try {
          const providers = await providersApi.listProviders(undefined, {noDedup: true})
          currentConfigured = providers.some(p => p.id === current && p.configured)
        } catch (err) {
          logger.warn('Failed to check the active provider', err)
          return
        }
        if (currentConfigured) return
      }
      setProvider(id)
      void updateAI({activeProvider: id})
    },
    [setProvider, updateAI],
  )

  const onUnavailable = useCallback((id: string) => {
    setHidden(prev => (prev.includes(id) ? prev : [...prev, id]))
  }, [])

  if (state.kind === 'legacy') return <LegacyProvidersPanel />
  if (state.kind === 'error') {
    return (
      <ErrorState
        title={t('providers.catalogLoadFailed')}
        message={state.message}
        onRetry={retry}
        retryLabel={t('providers.catalogRetry')}
      />
    )
  }
  if (state.kind === 'loading') return null

  const specs = state.specs.filter(spec => !hidden.includes(spec.id) && !(spec.id === 'local' && knownRemoteBackend))
  return (
    <div className="space-y-8">
      {specs.map(spec => (
        <ProviderSection
          key={spec.id}
          spec={spec}
          onConfigured={onConfigured}
          onActivated={(id, when) => void onActivated(id, when)}
          onUnavailable={onUnavailable}
        />
      ))}
    </div>
  )
}
