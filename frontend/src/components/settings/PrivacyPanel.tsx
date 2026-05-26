import Switch from '@/components/shared/Switch'
import {useSettingsStore} from '@/stores/settingsStore'

export default function PrivacyPanel() {
  const settings = useSettingsStore(s => s.settings)
  const updateSettings = useSettingsStore(s => s.updateSettings)

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">Privacy</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">
        Control how PAD Analyzer handles your data.
      </p>

      <div className="space-y-4">
        <div className="flex items-center justify-between py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
          <div>
            <span className="text-sm font-medium text-text-primary">Telemetry</span>
            <p className="text-xs text-text-tertiary mt-0.5">
              Send anonymous usage data to help improve PAD Analyzer.
            </p>
          </div>
          <Switch
            checked={settings.telemetry.enabled}
            onChange={(v) => updateSettings({telemetry: {...settings.telemetry, enabled: v}})}
          />
        </div>

        <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
          <span className="text-sm font-medium text-text-primary">API Keys</span>
          <p className="text-xs text-text-tertiary mt-0.5">
            All API keys are stored securely in your operating system's keychain.
            They are never sent to any server other than the respective AI provider.
          </p>
        </div>
      </div>
    </div>
  )
}
