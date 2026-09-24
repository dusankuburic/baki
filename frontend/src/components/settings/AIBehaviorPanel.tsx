import {useState} from 'react'
import {useTranslation} from 'react-i18next'
import {useSettingsStore} from '@/stores/settingsStore'
import {useChatStore} from '@/stores/chatStore'
import {useSystemStore} from '@/stores/systemStore'
import Switch from '@/components/shared/Switch'
import Input from '@/components/shared/Input'
import {NumberField} from '@/components/shared'
import SegmentedControl from '@/components/shared/SegmentedControl'
import {isTauri} from '@/platform/guards'
import type {ProviderID, AIProviderConfig} from '@/types'
import {PROVIDER_OPTIONS} from '@/types/providers'

export default function AIBehaviorPanel() {
  const {t} = useTranslation('settings')
  const ai = useSettingsStore(s => s.settings.ai)
  const updateAI = useSettingsStore(s => s.updateAI)
  const updateProvider = useSettingsStore(s => s.updateProvider)
  const activeProvider = ai.activeProvider
  // Local-mode backend (desktop sidecar OR a browser-served `PAD_MODE=local`
  // server) has no usage store: the daily-spend read returns 0 there, so a
  // budget never accumulates. /api/system/info reports that mode as
  // capabilities.sessionAnalytics; isTauri() is only the fallback until the
  // info has loaded — it missed the browser-served local case entirely.
  const systemInfo = useSystemStore(s => s.info)
  const localBackend = systemInfo ? systemInfo.capabilities.sessionAnalytics : isTauri()

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">{t('behavior.title')}</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">{t('behavior.subtitle')}</p>

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
            options={PROVIDER_OPTIONS}
            className="flex-wrap"
          />
          <p className="text-xs text-text-tertiary mt-2">{t('behavior.activeAssistantHint')}</p>
        </div>

        <div>
          <label className="text-sm font-medium text-text-primary block mb-3">{t('behavior.embeddingAssistant')}</label>
          <SegmentedControl
            value={ai.embeddingProvider}
            onChange={v => updateAI({embeddingProvider: v as ProviderID})}
            options={[
              {value: 'openai', label: 'OpenAI'},
              {value: 'gemini', label: 'Gemini'},
              {value: 'glm', label: 'GLM'},
              {value: 'github-models', label: 'GitHub Models'},
            ]}
          />
          <p className="text-xs text-text-tertiary mt-2">{t('behavior.embeddingAssistantHint')}</p>
        </div>

        <div>
          <label className="text-sm font-medium text-text-primary block mb-3">{t('behavior.embeddingModel')}</label>
          <Input
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

        <div className="space-y-4 pt-2">
          <label className="text-sm font-medium text-text-primary block">{t('behavior.historyCosts')}</label>

          <div className="flex items-center justify-between py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
            <div>
              <span className="text-sm font-medium text-text-primary">{t('behavior.saveHistory')}</span>
              <p className="text-xs text-text-tertiary mt-0.5">{t('behavior.saveHistoryHint')}</p>
            </div>
            <Switch
              label={t('behavior.saveHistory')}
              checked={ai.saveConversationHistory}
              onChange={v => updateAI({saveConversationHistory: v})}
            />
          </div>

          <div className="flex items-center justify-between py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
            <div>
              <span className="text-sm font-medium text-text-primary">{t('behavior.showCosts')}</span>
              <p className="text-xs text-text-tertiary mt-0.5">{t('behavior.showCostsHint')}</p>
            </div>
            <Switch
              label={t('behavior.showCosts')}
              checked={ai.showCostEstimates}
              onChange={v => updateAI({showCostEstimates: v})}
            />
          </div>

          {/* Local mode has no usage store: the server's daily-spend read
              returns 0 there, so a budget entered here was never enforced
              while looking exactly like the cloud setting. Hidden rather
              than shown inert. */}
          {!localBackend && (
            <div className="flex items-center justify-between py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
              <div className="flex-1">
                <span className="text-sm font-medium text-text-primary">{t('behavior.dailyBudget')}</span>
                <p className="text-xs text-text-tertiary mt-0.5">{t('behavior.dailyBudgetHint')}</p>
              </div>
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
            </div>
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
            config={ai.providers[activeProvider]}
            onUpdate={patch => updateProvider(activeProvider, patch)}
          />
        </div>
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
      className="w-full h-24 px-3 py-2 text-sm bg-surface-2 border border-border-default rounded-md text-text-primary placeholder:text-text-tertiary resize-none focus:outline-none focus:ring-1 focus:ring-brand-500"
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
  config,
  onUpdate,
}: {
  config: AIProviderConfig
  onUpdate: (patch: Partial<AIProviderConfig>) => void
}) {
  const {t} = useTranslation('settings')
  if (!config) return null

  return (
    <div className="grid grid-cols-2 gap-6">
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

      <div>
        <label className="text-xs font-medium text-text-secondary block mb-1.5">{t('behavior.defaultModel')}</label>
        <Input
          value={config.defaultModel}
          onChange={e => onUpdate({defaultModel: e.target.value})}
          placeholder={t('behavior.defaultModelPlaceholder')}
        />
      </div>
    </div>
  )
}
