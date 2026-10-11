import {useSettingsStore} from '@/stores/settingsStore'
import {NumberField} from '@/components/shared'
import {useTranslation} from 'react-i18next'

export default function ParserPanel() {
  const {t} = useTranslation('settings')
  const parser = useSettingsStore(s => s.settings.parser)
  const updateParser = useSettingsStore(s => s.updateParser)

  return (
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
  )
}
