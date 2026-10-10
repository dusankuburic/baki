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
  // A server without the route (404): the built-in sections.
  | {kind: 'legacy'}
  | {kind: 'error'; message: string}

// AI Accounts: one section per entry of GET /api/providers/catalog, in the
// server's order, each rendering the credential form its schema asks for
// (ProviderSection). The catalogue lists unconfigured providers too, so a
// form shows before anything is saved. A server older than this bundle has
// no catalogue: the panel renders the sections it always had.
export default function ProvidersPanel() {
  const {t} = useTranslation('settings')
  const [state, setState] = useState<CatalogState>({kind: 'loading'})
  // Sections the server declined after the fact (the local model on a cloud
  // server answers LOCAL_MODELS_UNAVAILABLE to its own GET).
  const [hidden, setHidden] = useState<string[]>([])
  const alive = useRef(true)
  const bumpProviderEpoch = useChatStore(s => s.bumpProviderEpoch)
  const setProvider = useChatStore(s => s.setProvider)
  const updateAI = useSettingsStore(s => s.updateAI)
  // The server's catalogue is already filtered by mode; this keeps the local
  // section from flashing before its own GET is refused on a cloud server.
  const knownRemoteBackend = useSystemStore(s => (s.info ? !s.info.capabilities.sessionAnalytics : false))

  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  // noDedup: a reopen right after a save must not read the 5 s GET micro-cache's pre-change view.
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

  // A configuration changed (key saved or removed, sign-in completed or
  // revoked): AITab re-checks the providers.
  const onConfigured = useCallback(() => bumpProviderEpoch(), [bumpProviderEpoch])

  // A sign-in makes the provider the active one, so the chat is usable
  // without a manual switch. A first model (Azure) does so only when the
  // current active provider cannot chat, so a working setup is never
  // switched away from; a failed lookup leaves the provider as it is.
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
