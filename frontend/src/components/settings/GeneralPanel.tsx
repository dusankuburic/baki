import {useSettingsStore} from '@/stores/settingsStore'
import {useTranslation} from 'react-i18next'

export default function GeneralPanel() {
  const {t} = useTranslation('settings')
  const updateGeneral = useSettingsStore(s => s.updateGeneral)

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">General</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">{t('general.subtitle')}</p>

      <div className="space-y-6">
        <div className="p-4 rounded-lg bg-surface-2 border border-border-default">
          <h3 className="text-sm font-medium text-text-primary">Updates</h3>
          <p className="text-xs text-text-tertiary mt-1">
            PAD Analyzer is under active development. Check the releases page for the latest version.
          </p>
          <a
            className="mt-3 inline-block text-xs font-medium text-accent-blue hover:underline"
            href="https://github.com/sftfox/pad-analyzer/releases"
            target="_blank"
            rel="noreferrer"
          >
            View releases →
          </a>
        </div>

        <div className="p-4 rounded-lg bg-surface-2 border border-border-default">
          <h3 className="text-sm font-medium text-text-primary">Onboarding</h3>
          <p className="text-xs text-text-tertiary mt-1">
            Replay the welcome tour and guided walkthrough.
          </p>
          <button
            className="mt-3 text-xs font-medium text-accent-blue hover:underline"
            onClick={() => updateGeneral({firstRunCompleted: false})}
          >
            Replay tour →
          </button>
        </div>
      </div>
    </div>
  )
}
