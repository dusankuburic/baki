import {useState, useEffect, useCallback} from 'react'
import ApiKeyInput from '../ApiKeyInput'
import AzureFoundryPanel from '../azure/AzureFoundryPanel'
import {AZURE_SECTION_ANCHOR} from '../azure/registryView'
import LocalModelsSection from '../LocalModelsSection'
import {ProviderHeading as ProviderSection} from './ProviderSection'
import GitHubLoginButton from '@/components/chat/GitHubLoginButton'
import CopilotLoginButton from '@/components/chat/CopilotLoginButton'
import {providersApi} from '@/api'
import {logger} from '@/lib/logger'
import {useChatStore} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useSystemStore} from '@/stores/systemStore'
import type {AzureScope} from '@/types'
import {useTranslation} from 'react-i18next'

const USER_SCOPE: AzureScope = {kind: 'user'}

// The built-in sections, for a server without GET /api/providers/catalog
// (one older than this bundle): the nine providers the bundle knows, each
// with its own widget, exactly as before the catalogue. Goes with the
// catalogue's second release.
export default function LegacyProvidersPanel() {
  const {t} = useTranslation('settings')
  const [githubUser, setGithubUser] = useState<string | null>(null)
  const [copilotUser, setCopilotUser] = useState<string | null>(null)
  const bumpProviderEpoch = useChatStore(s => s.bumpProviderEpoch)
  const setProvider = useChatStore(s => s.setProvider)
  const updateAI = useSettingsStore(s => s.updateAI)
  // A cloud server does not offer local models: the section is not rendered
  // once /api/system/info says the backend is not local (the signal the rest
  // of Settings uses: capabilities.sessionAnalytics), and before that info
  // loads the section's own GET answers LOCAL_MODELS_UNAVAILABLE, which it
  // reports so the heading goes with it.
  const [localUnavailable, setLocalUnavailable] = useState(false)
  const markLocalUnavailable = useCallback(() => setLocalUnavailable(true), [])
  const knownRemoteBackend = useSystemStore(s => (s.info ? !s.info.capabilities.sessionAnalytics : false))

  // refreshLocal reloads the signed-in OAuth users without bumping the
  // provider epoch. Used for the initial mount load (no config changed yet);
  // the key inputs and the Azure section load their own state.
  const refreshLocal = useCallback(() => {
    providersApi
      .getGitHubUser()
      .then((u: {login?: string} | null) => setGithubUser(u?.login || null))
      .catch(() => setGithubUser(null))
    providersApi
      .getCopilotUser()
      .then((u: {login?: string} | null) => setCopilotUser(u?.login || null))
      .catch(() => setCopilotUser(null))
  }, [])

  // refresh is refreshLocal + epoch bump. Used when a configuration actually
  // changes (API key saved, OAuth completed, auth revoked) so AITab re-checks.
  const refresh = useCallback(() => {
    refreshLocal()
    bumpProviderEpoch()
  }, [refreshLocal, bumpProviderEpoch])

  // handleCopilotAuthComplete / handleGithubAuthComplete: called by the device-
  // flow login buttons when OAuth succeeds. Refreshes state + switches the
  // active provider so the chat is immediately usable without manual selection.
  const handleCopilotAuthComplete = useCallback(() => {
    refresh()
    setProvider('copilot')
    void updateAI({activeProvider: 'copilot'})
  }, [refresh, setProvider, updateAI])

  const handleGithubAuthComplete = useCallback(() => {
    refresh()
    setProvider('github-models')
    void updateAI({activeProvider: 'github-models'})
  }, [refresh, setProvider, updateAI])

  // The first Azure AI Foundry chat model makes Azure the active provider —
  // but only when the current one cannot chat (not configured), so a working
  // setup is never switched away from. The registry store already relisted
  // the chat picker. A failed lookup leaves the provider as it is.
  const handleFirstAzureChatModel = useCallback(async () => {
    const current = useSettingsStore.getState().settings.ai.activeProvider
    if (current === 'azure-foundry') return
    let currentConfigured: boolean
    try {
      const providers = await providersApi.listProviders(undefined, {noDedup: true})
      currentConfigured = providers.some(p => p.id === current && p.configured)
    } catch (err) {
      logger.warn('Failed to check the active provider', err)
      return
    }
    if (currentConfigured) return
    setProvider('azure-foundry')
    void updateAI({activeProvider: 'azure-foundry'})
  }, [setProvider, updateAI])

  useEffect(() => {
    refreshLocal()
  }, [refreshLocal])

  return (
    <div className="space-y-8">
      <ProviderSection id="claude" name="Claude" color="#d4a574">
        <ApiKeyInput provider="claude" label={t('providers.anthropicKey')} onConfigured={refresh} />
      </ProviderSection>

      <ProviderSection id="openai" name="OpenAI" color="#10a37f">
        <ApiKeyInput provider="openai" label={t('providers.openaiKey')} onConfigured={refresh} />
      </ProviderSection>

      <ProviderSection id="gemini" name="Gemini" color="#4285f4">
        <ApiKeyInput provider="gemini" label={t('providers.googleKey')} onConfigured={refresh} />
      </ProviderSection>

      <ProviderSection id="xai" name="xAI (Grok)" color="#f43f5e">
        <ApiKeyInput provider="xai" label={t('providers.xaiKey')} onConfigured={refresh} />
      </ProviderSection>

      <ProviderSection id="glm" name="GLM (z.ai)" color="#06b6d4">
        <ApiKeyInput provider="glm" label={t('providers.zaiKey')} onConfigured={refresh} />
      </ProviderSection>

      {/* Its heading is the "Manage models…" deep link's anchor
          (AZURE_SECTION_ANCHOR = provider-azure-foundry): the panel focuses
          it once its resources and models have loaded. */}
      <ProviderSection id="azure-foundry" name="Azure AI Foundry" color="#0078d4">
        <AzureFoundryPanel
          scope={USER_SCOPE}
          headingLevel={4}
          anchorId={AZURE_SECTION_ANCHOR}
          onFirstChatModel={() => void handleFirstAzureChatModel()}
        />
      </ProviderSection>

      {!localUnavailable && !knownRemoteBackend && (
        <ProviderSection id="local" name={t('providers.localName')} color="#f59e0b">
          <LocalModelsSection onConfigured={refresh} onUnavailable={markLocalUnavailable} />
        </ProviderSection>
      )}

      <ProviderSection id="github-models" name="GitHub Models" color="#8b5cf6">
        <div className="space-y-2">
          {githubUser ? (
            <div className="flex items-center gap-3">
              <span className="text-sm text-text-secondary">
                {t('providers.connectedAs')} <strong className="text-text-primary">@{githubUser}</strong>
              </span>
              <button
                className="text-xs text-red-400 hover:text-red-300"
                onClick={() =>
                  providersApi
                    .revokeGitHubAuth()
                    .then(() => refresh())
                    .catch(err => {
                      logger.warn('Failed to revoke GitHub auth', err)
                    })
                }
              >
                {t('providers.disconnect')}
              </button>
            </div>
          ) : (
            <GitHubLoginButton onAuthComplete={handleGithubAuthComplete} />
          )}
        </div>
      </ProviderSection>

      <ProviderSection id="copilot" name="GitHub Copilot" color="#6e40c9">
        <div className="space-y-2.5">
          <p className="text-xs text-text-tertiary leading-relaxed">
            {t('providers.copilotRequires')}{' '}
            <a
              href="https://github.com/features/copilot"
              target="_blank"
              rel="noopener noreferrer"
              className="text-brand-400 hover:underline"
            >
              {t('providers.copilotSubscriptionLink')}
            </a>
            .
          </p>
          {copilotUser ? (
            <div className="flex items-center gap-3">
              <span className="text-sm text-text-secondary">
                {t('providers.connectedAs')} <strong className="text-text-primary">@{copilotUser}</strong>
              </span>
              <button
                className="text-xs text-red-400 hover:text-red-300"
                onClick={() =>
                  providersApi
                    .revokeCopilotAuth()
                    .then(() => refresh())
                    .catch(err => {
                      logger.warn('Failed to revoke Copilot auth', err)
                    })
                }
              >
                {t('providers.disconnect')}
              </button>
            </div>
          ) : (
            <CopilotLoginButton onAuthComplete={handleCopilotAuthComplete} />
          )}
          {!copilotUser && (
            <details className="text-xs text-text-tertiary">
              <summary className="cursor-pointer hover:text-text-secondary transition-colors">
                {t('providers.orUsePat')}
              </summary>
              <div className="mt-2">
                <ApiKeyInput provider="copilot" label={t('providers.githubPat')} onConfigured={refresh} />
              </div>
            </details>
          )}
        </div>
      </ProviderSection>
    </div>
  )
}
