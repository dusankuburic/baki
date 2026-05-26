import SegmentedControl from '@/components/shared/SegmentedControl'
import {useSettingsStore} from '@/stores/settingsStore'
import type {ThemeMode} from '@/types/domain'

export default function AppearancePanel() {
  const theme = useSettingsStore(s => s.settings.appearance.theme)
  const updateAppearance = useSettingsStore(s => s.updateAppearance)

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">Appearance</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">
        Customize the look and feel of PAD Analyzer.
      </p>

      <div className="space-y-6">
        <div>
          <label className="text-sm font-medium text-text-primary block mb-2">Theme</label>
          <SegmentedControl
            value={theme}
            onChange={(v) => updateAppearance({theme: v as ThemeMode})}
            options={[
              {value: 'dark', label: 'Dark'},
              {value: 'light', label: 'Light'},
              {value: 'system', label: 'System'},
            ]}
          />
        </div>
      </div>
    </div>
  )
}
