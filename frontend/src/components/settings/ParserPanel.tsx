import {useSettingsStore} from '@/stores/settingsStore'
import {NumberField} from '@/components/shared'
import {useTranslation} from 'react-i18next'

// The panel used to also offer "Preserve comments", "Treat tabs as spaces" and
// "Spaces per indent". They were saved with the settings and never read by the
// parser, so toggling them changed nothing; the size cap is the one parser
// setting the backend consults.
export default function ParserPanel() {
  const {t} = useTranslation('settings')
  const parser = useSettingsStore(s => s.settings.parser)
  const updateParser = useSettingsStore(s => s.updateParser)

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">Parser</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">{t('parser.subtitle')}</p>

      <div className="space-y-6">
        <div>
          <label className="text-sm font-medium text-text-primary block mb-2">{t('parser.maxFileSize')}</label>
          <div className="w-32">
            <NumberField
              min={1}
              max={500}
              fallback={50}
              value={parser.maxFileSizeMB}
              onCommit={v => updateParser({maxFileSizeMB: v})}
            />
          </div>
          <p className="text-xs text-text-tertiary mt-2">{t('parser.maxFileSizeHint')}</p>
        </div>
      </div>
    </div>
  )
}
