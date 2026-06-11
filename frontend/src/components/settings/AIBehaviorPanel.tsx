import {useState} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'
import Switch from '@/components/shared/Switch'
import Input from '@/components/shared/Input'
import SegmentedControl from '@/components/shared/SegmentedControl'
import type {ProviderID} from '@/types/domain'

export default function AIBehaviorPanel() {
  const {settings, updateAI, updateProvider} = useSettingsStore()
  const ai = settings.ai
  const activeProvider = ai.activeProvider

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">AI Behavior</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">
        Configure how AI assistants interact with your flows.
      </p>

      <div className="space-y-8">
        <div>
          <label className="text-sm font-medium text-text-primary block mb-3">Active Assistant</label>
          <SegmentedControl
            value={activeProvider}
            onChange={(v) => updateAI({activeProvider: v as ProviderID})}
            options={[
              {value: 'claude', label: 'Claude'},
              {value: 'openai', label: 'OpenAI'},
              {value: 'gemini', label: 'Gemini'},
              {value: 'copilot', label: 'Copilot'},
            ]}
          />
          <p className="text-xs text-text-tertiary mt-2">
            Select the primary AI provider to use for chat and analysis.
          </p>
        </div>

        <div>
          <label className="text-sm font-medium text-text-primary block mb-3">Embedding Assistant</label>
          <SegmentedControl
            value={ai.embeddingProvider}
            onChange={(v) => updateAI({embeddingProvider: v as ProviderID})}
            options={[
              {value: 'openai', label: 'OpenAI'},
              {value: 'gemini', label: 'Gemini'},
            ]}
          />
          <p className="text-xs text-text-tertiary mt-2">
            Provider used to index and search your Knowledge Base.
          </p>
        </div>

        <div className="space-y-4 pt-2">
          <label className="text-sm font-medium text-text-primary block">History & Costs</label>
          
          <div className="flex items-center justify-between py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
            <div>
              <span className="text-sm font-medium text-text-primary">Save Conversation History</span>
              <p className="text-xs text-text-tertiary mt-0.5">
                Keep a local record of your AI chats per flow.
              </p>
            </div>
            <Switch
              checked={ai.saveConversationHistory}
              onChange={(v) => updateAI({saveConversationHistory: v})}
            />
          </div>

          <div className="flex items-center justify-between py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
            <div>
              <span className="text-sm font-medium text-text-primary">Show Cost Estimates</span>
              <p className="text-xs text-text-tertiary mt-0.5">
                Display estimated token usage and costs for each request.
              </p>
            </div>
            <Switch
              checked={ai.showCostEstimates}
              onChange={(v) => updateAI({showCostEstimates: v})}
            />
          </div>

          <div className="flex items-center justify-between py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
            <div className="flex-1">
              <span className="text-sm font-medium text-text-primary">Daily AI Budget ($)</span>
              <p className="text-xs text-text-tertiary mt-0.5">
                Max spend per day for your account/org. Use 0 for unlimited.
              </p>
            </div>
            <div className="w-24">
              <Input
                type="number"
                step="0.5"
                min="0"
                value={ai.dailyBudget}
                onChange={(e) => updateAI({dailyBudget: parseFloat(e.target.value) || 0})}
              />
            </div>
          </div>
        </div>

        <div className="pt-2">
          <h3 className="text-base font-semibold text-text-primary mb-1">Custom Instructions</h3>
          <p className="text-xs text-text-tertiary mb-3">
            Appended to the AI system prompt. Use this to set a preferred language, focus area, or persona.
          </p>
          <CustomInstructionsInput />
        </div>

        <div className="pt-2 space-y-6">
          <h3 className="text-base font-semibold text-text-primary mb-1">Advanced Provider Settings</h3>
          <ProviderAdvancedSettings 
            config={ai.providers[activeProvider]} 
            onUpdate={(patch) => updateProvider(activeProvider, patch)}
          />
        </div>
      </div>
    </div>
  )
}

function CustomInstructionsInput() {
  const suffix = useSettingsStore(s => s.settings.ai.systemPromptSuffix ?? '')
  const updateAI = useSettingsStore(s => s.updateAI)
  const [local, setLocal] = useState(suffix)

  const handleBlur = () => {
    if (local !== suffix) {
      updateAI({systemPromptSuffix: local})
    }
  }

  return (
    <textarea
      className="w-full h-24 px-3 py-2 text-sm bg-surface-2 border border-border-default rounded-md text-text-primary placeholder:text-text-tertiary resize-none focus:outline-none focus:ring-1 focus:ring-brand-500"
      placeholder="e.g. Always respond in Serbian. Focus on security and error handling."
      value={local}
      onChange={e => setLocal(e.target.value)}
      onBlur={handleBlur}
    />
  )
}

function ProviderAdvancedSettings({config, onUpdate}: {config: any; onUpdate: (patch: any) => void}) {
  if (!config) return null

  return (
    <div className="grid grid-cols-2 gap-6">
      <div className="space-y-1.5">
        <label className="text-xs font-medium text-text-secondary">Temperature ({config.temperature})</label>
        <input
          type="range"
          min="0"
          max="1"
          step="0.1"
          className="w-full accent-brand-500"
          value={config.temperature}
          onChange={(e) => onUpdate({temperature: parseFloat(e.target.value)})}
        />
        <div className="flex justify-between text-2xs text-text-tertiary">
          <span>Precise</span>
          <span>Creative</span>
        </div>
      </div>

      <div>
        <label className="text-xs font-medium text-text-secondary block mb-1.5">Max Tokens</label>
        <Input
          type="number"
          min={1}
          max={32000}
          value={config.maxTokens}
          onChange={(e) => onUpdate({maxTokens: parseInt(e.target.value) || 4096})}
        />
      </div>

      <div>
        <label className="text-xs font-medium text-text-secondary block mb-1.5">Context Token Budget</label>
        <Input
          type="number"
          min={100}
          max={128000}
          value={config.contextTokenBudget}
          onChange={(e) => onUpdate({contextTokenBudget: parseInt(e.target.value) || 4000})}
        />
        <p className="text-2xs text-text-tertiary mt-1">
          Max tokens used for flow context.
        </p>
      </div>

      <div>
        <label className="text-xs font-medium text-text-secondary block mb-1.5">Default Model ID</label>
        <Input
          value={config.defaultModel}
          onChange={(e) => onUpdate({defaultModel: e.target.value})}
          placeholder="e.g. gpt-4o"
        />
      </div>
    </div>
  )
}
