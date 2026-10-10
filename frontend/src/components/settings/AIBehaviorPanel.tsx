import {useEffect, useMemo, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {providerConfig, useSettingsStore} from '@/stores/settingsStore'
import {useChatStore} from '@/stores/chatStore'
import {useSystemStore} from '@/stores/systemStore'
import {providersApi} from '@/api'
import Switch from '@/components/shared/Switch'
import Input from '@/components/shared/Input'
import {NumberField} from '@/components/shared'
import SegmentedControl from '@/components/shared/SegmentedControl'
import SettingRow from './SettingRow'
import {isTauri} from '@/platform/guards'
import type {ProviderID, AIProviderConfig, ProviderSpec} from '@/types'
import {PROVIDER_CAP_EMBEDDINGS, PROVIDER_OPTIONS, isKnownProviderId} from '@/types/providers'

// The embedding-capable providers of a server without the catalogue (one
// older than Phase 4b): the server's SupportsEmbeddings list of that time.
const LEGACY_EMBEDDING_OPTIONS: {value: ProviderID; label: string}[] = [
  {value: 'openai', label: 'OpenAI'},
  {value: 'gemini', label: 'Gemini'},
  {value: 'glm', label: 'GLM'},
  {value: 'github-models', label: 'GitHub Models'},
  {value: 'azure-foundry', label: 'Azure AI Foundry'},
]

export default function AIBehaviorPanel() {
  const {t} = useTranslation('settings')
  const ai = useSettingsStore(s => s.settings.ai)
  const updateAI = useSettingsStore(s => s.updateAI)
  const updateProvider = useSettingsStore(s => s.updateProvider)
  const activeProvider = ai.activeProvider
  // On Azure AI Foundry the knowledge base's embedding model is a registry
  // designation ("Use for knowledge base" in AI Accounts), not a free-text
  // override: the field gives way to a pointer there.
  const azureEmbedding = ai.embeddingProvider === 'azure-foundry'
  // Local-mode backend (desktop sidecar OR a browser-served `PAD_MODE=local`
  // server) has no usage store: the daily-spend read returns 0 there, so a
  // budget never accumulates. /api/system/info reports that mode as
  // capabilities.sessionAnalytics; isTauri() is only the fallback until the
  // info has loaded — it missed the browser-served local case entirely.
  const systemInfo = useSystemStore(s => s.info)
  const localBackend = systemInfo ? systemInfo.capabilities.sessionAnalytics : isTauri()
  // The server's catalogue (GET /api/providers/catalog): the names of the
  // providers this bundle has no label for (ones a newer server added), and
  // which providers embed. Best effort: without it (an older server, or a
  // failed read) the bundle's own lists apply, and an unknown provider is
  // labelled by its id.
  const [catalog, setCatalog] = useState<ProviderSpec[] | null>(null)
  useEffect(() => {
    let cancelled = false
    providersApi
      .getProviderCatalog()
      .then(specs => {
        if (!cancelled) setCatalog(specs)
      })
      .catch(() => {
        // The bundle's own labels and lists still render.
      })
    return () => {
      cancelled = true
    }
  }, [])
  // The local model provider exists only on a local backend (the server
  // refuses it elsewhere, LOCAL_MODELS_UNAVAILABLE): offering it on a cloud
  // server picked an assistant the chat could never use. After the
  // catalogue: the providers the server lists but the bundle does not know,
  // under their server names — and the saved choice itself when it is
  // neither, under its id, so a settings file a newer build (or a hand)
  // wrote still shows what it selected instead of nothing.
  const assistantOptions = useMemo(() => {
    const options: {value: ProviderID; label: string}[] = localBackend
      ? [...PROVIDER_OPTIONS]
      : PROVIDER_OPTIONS.filter(o => o.value !== 'local')
    for (const p of catalog ?? []) {
      if (!isKnownProviderId(p.id) && p.id && !options.some(o => o.value === p.id)) {
        options.push({value: p.id, label: p.name || p.id})
      }
    }
    if (activeProvider && !options.some(o => o.value === activeProvider)) {
      options.push({value: activeProvider, label: activeProvider})
    }
    return options
  }, [localBackend, catalog, activeProvider])

  // The embedding providers: the catalogue's `embeddings` capability, in
  // its order and under its names; the literal list without a catalogue.
  // The saved choice stays selectable, under its id, when the server does
  // not list it at all (a provider it dropped) rather than selecting
  // nothing; one the catalogue lists without the capability is not offered —
  // the server would refuse it.
  const embeddingOptions = useMemo(() => {
    const options: {value: ProviderID; label: string}[] = catalog
      ? catalog.filter(p => p.caps.includes(PROVIDER_CAP_EMBEDDINGS)).map(p => ({value: p.id, label: p.name || p.id}))
      : [...LEGACY_EMBEDDING_OPTIONS]
    const saved = ai.embeddingProvider
    if (saved && !options.some(o => o.value === saved) && !catalog?.some(p => p.id === saved)) {
      options.push({value: saved, label: saved})
    }
    return options
  }, [catalog, ai.embeddingProvider])

  return (
    <div className="space-y-8">
      <div>
        <label className="text-sm font-medium text-text-primary block mb-3">{t('behavior.activeAssistant')}</label>
        <SegmentedControl
          value={activeProvider}
          onChange={v => {
            void updateAI({activeProvider: v})
            // The chat reads its provider from the chat store, which was
            // only seeded from settings at start-up: picking an assistant
            // here did nothing until a reload (the Advanced block below then
            // tuned a provider the chat wasn't using).
            useChatStore.getState().setProvider(v)
          }}
          options={assistantOptions}
          activation="manual"
          className="flex-wrap"
        />
        <p className="text-xs text-text-tertiary mt-2">{t('behavior.activeAssistantHint')}</p>
      </div>

      <div>
        <label className="text-sm font-medium text-text-primary block mb-3">{t('behavior.embeddingAssistant')}</label>
        {/* Embedding-capable providers only (the catalogue's caps, or the
            server's SupportsEmbeddings list of before it): never 'local',
            whose provider cannot embed. */}
        <SegmentedControl
          value={ai.embeddingProvider}
          onChange={v => updateAI({embeddingProvider: v})}
          label={t('behavior.embeddingAssistant')}
          activation="manual"
          options={embeddingOptions}
          className="flex-wrap"
        />
        <p className="text-xs text-text-tertiary mt-2">{t('behavior.embeddingAssistantHint')}</p>
      </div>

      {azureEmbedding ? (
        <div>
          <p className="text-sm font-medium text-text-primary mb-3">{t('behavior.embeddingModel')}</p>
          <p className="text-xs text-text-tertiary">{t('behavior.embeddingModelHintAzure')}</p>
        </div>
      ) : (
        <div>
          <label htmlFor="embedding-model" className="text-sm font-medium text-text-primary block mb-3">
            {t('behavior.embeddingModel')}
          </label>
          <Input
            id="embedding-model"
            value={ai.embeddingModel ?? ''}
            onChange={e => updateAI({embeddingModel: e.target.value})}
            placeholder={t('behavior.embeddingModelPlaceholder')}
          />
          <p className="text-xs text-text-tertiary mt-2">
            {/* Split-rendered so the model name keeps its inline <code> style
                  without a Trans dependency (typed Trans + components is brittle). */}
            {t('behavior.embeddingModelHintPrefix')} <code className="text-text-secondary">text-embedding-3-large</code>
            {t('behavior.embeddingModelHintSuffix')}
          </p>
        </div>
      )}

      <div className="space-y-4 pt-2">
        <label className="text-sm font-medium text-text-primary block">{t('behavior.historyCosts')}</label>

        <SettingRow
          title={t('behavior.saveHistory')}
          hint={t('behavior.saveHistoryHint')}
          control={
            <Switch
              label={t('behavior.saveHistory')}
              labelHidden
              checked={ai.saveConversationHistory}
              onChange={v => updateAI({saveConversationHistory: v})}
            />
          }
        />

        <SettingRow
          title={t('behavior.showCosts')}
          hint={t('behavior.showCostsHint')}
          control={
            <Switch
              label={t('behavior.showCosts')}
              labelHidden
              checked={ai.showCostEstimates}
              onChange={v => updateAI({showCostEstimates: v})}
            />
          }
        />

        {/* Local mode has no usage store: the server's daily-spend read
              returns 0 there, so a budget entered here was never enforced
              while looking exactly like the cloud setting. Hidden rather
              than shown inert. */}
        {!localBackend && (
          <SettingRow
            title={t('behavior.dailyBudget')}
            hint={t('behavior.dailyBudgetHint')}
            control={
              <div className="w-24">
                <NumberField
                  step="0.5"
                  min={0}
                  fallback={0}
                  integer={false}
                  value={ai.dailyBudget}
                  onCommit={v => updateAI({dailyBudget: v})}
                />
              </div>
            }
          />
        )}
      </div>

      <div className="pt-2">
        <h3 className="text-base font-semibold text-text-primary mb-1">{t('behavior.customInstructions')}</h3>
        <p className="text-xs text-text-tertiary mb-3">{t('behavior.customInstructionsHint')}</p>
        <CustomInstructionsInput />
      </div>

      <div className="pt-2 space-y-6">
        <h3 className="text-base font-semibold text-text-primary mb-1">{t('behavior.advanced')}</h3>
        <ProviderAdvancedSettings
          provider={activeProvider}
          config={providerConfig(ai, activeProvider)}
          onUpdate={patch => updateProvider(activeProvider, patch)}
        />
      </div>
    </div>
  )
}

function CustomInstructionsInput() {
  const {t} = useTranslation('settings')
  const suffix = useSettingsStore(s => s.settings.ai.systemPromptSuffix ?? '')
  const updateAI = useSettingsStore(s => s.updateAI)
  // A local draft keeps typing smooth; every change is written through (the
  // store debounces the backend save). Saving only on blur lost the text when
  // the dialog was closed with Escape: React fires no blur for an element that
  // unmounts while focused.
  const [local, setLocal] = useState(suffix)

  return (
    <textarea
      className="w-full h-24 px-3 py-2 text-sm bg-surface-2 border border-border-default rounded-md text-text-primary placeholder:text-text-tertiary resize-none focus:outline-hidden focus:ring-1 focus:ring-brand-500"
      placeholder={t('behavior.customInstructionsPlaceholder')}
      aria-label={t('behavior.customInstructions')}
      value={local}
      onChange={e => {
        setLocal(e.target.value)
        void updateAI({systemPromptSuffix: e.target.value})
      }}
    />
  )
}

function ProviderAdvancedSettings({
  provider,
  config,
  onUpdate,
}: {
  provider: ProviderID
  config: AIProviderConfig
  onUpdate: (patch: Partial<AIProviderConfig>) => void
}) {
  const {t} = useTranslation('settings')
  if (!config) return null

  return (
    <div className="grid gap-6 grid-cols-[repeat(auto-fit,minmax(18rem,1fr))]">
      <div className="space-y-1.5">
        <label className="text-xs font-medium text-text-secondary">
          {t('behavior.temperature', {value: config.temperature})}
        </label>
        <input
          type="range"
          min="0"
          max="1"
          step="0.1"
          className="w-full accent-brand-500"
          value={config.temperature}
          onChange={e => onUpdate({temperature: parseFloat(e.target.value)})}
        />
        <div className="flex justify-between text-2xs text-text-tertiary">
          <span>{t('behavior.precise')}</span>
          <span>{t('behavior.creative')}</span>
        </div>
      </div>

      <div>
        <label className="text-xs font-medium text-text-secondary block mb-1.5">{t('behavior.maxTokens')}</label>
        <NumberField
          min={1}
          max={32000}
          fallback={4096}
          value={config.maxTokens}
          onCommit={v => onUpdate({maxTokens: v})}
        />
        {/* The server silently raises this to the 16384-token
            reasoning floor on reasoning models — say so instead of letting a
            lowered cap look ignored. Deliberately no clamp: the setting is
            per provider and non-reasoning deployments keep the lower cap. */}
        <p className="text-2xs text-text-tertiary mt-1">
          {t('behavior.maxTokensHint', {floor: (16384).toLocaleString()})}
        </p>
      </div>

      <div>
        <label className="text-xs font-medium text-text-secondary block mb-1.5">{t('behavior.contextBudget')}</label>
        <NumberField
          min={100}
          max={128000}
          fallback={4000}
          value={config.contextTokenBudget}
          onCommit={v => onUpdate({contextTokenBudget: v})}
        />
        <p className="text-2xs text-text-tertiary mt-1">{t('behavior.contextBudgetHint')}</p>
      </div>

      {/* Always visible — the panel cannot know which deployments are
          reasoning models (the served model is server-side knowledge), so
          the hint says who it applies to instead of a client-side regex. */}
      <div>
        <label htmlFor="reasoning-effort" className="text-xs font-medium text-text-secondary block mb-1.5">
          {t('behavior.reasoningEffort')}
        </label>
        <select
          id="reasoning-effort"
          className="w-full rounded border border-border-subtle bg-surface-1 px-2 py-1.5 text-sm text-text-primary"
          value={config.reasoningEffort ?? ''}
          onChange={e => onUpdate({reasoningEffort: e.target.value as AIProviderConfig['reasoningEffort']})}
        >
          <option value="">{t('behavior.reasoningEffortDefault')}</option>
          <option value="low">{t('behavior.reasoningEffortLow')}</option>
          <option value="medium">{t('behavior.reasoningEffortMedium')}</option>
          <option value="high">{t('behavior.reasoningEffortHigh')}</option>
        </select>
        <p className="text-2xs text-text-tertiary mt-1">{t('behavior.reasoningEffortHint')}</p>
      </div>

      {/* "Default for new chats": new chats start on it, and each chat then
          keeps its own model. For azure-foundry it is set with Make default
          in AI Accounts — registry ids are not typed by hand. */}
      {provider === 'azure-foundry' ? (
        <div>
          <p className="text-xs font-medium text-text-secondary mb-1.5">{t('behavior.defaultModel')}</p>
          <p className="text-2xs text-text-tertiary">{t('behavior.defaultModelAzureHint')}</p>
        </div>
      ) : (
        <div>
          <label htmlFor="default-model" className="text-xs font-medium text-text-secondary block mb-1.5">
            {t('behavior.defaultModel')}
          </label>
          <Input
            id="default-model"
            value={config.defaultModel}
            onChange={e => onUpdate({defaultModel: e.target.value})}
            placeholder={t('behavior.defaultModelPlaceholder')}
            hint={t('behavior.defaultModelHint')}
          />
        </div>
      )}
    </div>
  )
}
