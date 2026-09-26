import {useSettingsStore} from '@/stores/settingsStore'
import {useTranslation} from 'react-i18next'

export default function GeneralPanel() {
  const {t} = useTranslation('settings')
  const updateGeneral = useSettingsStore(s => s.updateGeneral)

  return (
    <div className="space-y-6">
      <div className="p-4 rounded-lg bg-surface-2 border border-border-default">
        <h3 className="text-sm font-medium text-text-primary">{t('general.updatesHeading')}</h3>
        <p className="text-xs text-text-tertiary mt-1">{t('general.updatesBody')}</p>
        {/* The project's own releases page. This pointed at an unrelated
              repository (sftfox/pad-analyzer), so "View releases" led nowhere. */}
        <a
          className="mt-3 inline-block text-xs font-medium text-brand-400 hover:underline"
          href="https://github.com/dusankuburic/baki/releases"
          target="_blank"
          rel="noreferrer"
        >
          {t('general.viewReleases')}
        </a>
      </div>

      <div className="p-4 rounded-lg bg-surface-2 border border-border-default">
        <h3 className="text-sm font-medium text-text-primary">{t('general.onboardingHeading')}</h3>
        <p className="text-xs text-text-tertiary mt-1">{t('general.onboardingBody')}</p>
        <button
          className="mt-3 text-xs font-medium text-brand-400 hover:underline"
          onClick={() => updateGeneral({firstRunCompleted: false})}
        >
          {t('general.replayTour')}
        </button>
      </div>
    </div>
  )
}
