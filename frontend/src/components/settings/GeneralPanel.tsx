import Switch from '@/components/shared/Switch'
import {useSettingsStore} from '@/stores/settingsStore'
import SegmentedControl from '@/components/shared/SegmentedControl'

export default function GeneralPanel() {
  const {checkForUpdates, openInNewWindow} = useSettingsStore(s => s.settings.general)
  const updateGeneral = useSettingsStore(s => s.updateGeneral)

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">General</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">Basic application settings and behavior.</p>

      <div className="space-y-8">
        <div>
          <label className="text-sm font-medium text-text-primary block mb-2">Check for Updates</label>
          <SegmentedControl
            value={checkForUpdates}
            onChange={v => updateGeneral({checkForUpdates: v as 'never' | 'daily' | 'weekly' | 'monthly'})}
            options={[
              {value: 'daily', label: 'Daily'},
              {value: 'weekly', label: 'Weekly'},
              {value: 'monthly', label: 'Monthly'},
              {value: 'never', label: 'Never'},
            ]}
          />
          <p className="text-xs text-text-tertiary mt-2">How often PAD Analyzer should check for new versions.</p>
        </div>

        <div className="pt-2">
          <div className="flex items-center justify-between py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
            <div>
              <span className="text-sm font-medium text-text-primary">Open in New Window</span>
              <p className="text-xs text-text-tertiary mt-0.5">
                Always open new flows in a separate application window.
              </p>
            </div>
            <Switch checked={openInNewWindow} onChange={v => updateGeneral({openInNewWindow: v})} />
          </div>
        </div>
      </div>
    </div>
  )
}
