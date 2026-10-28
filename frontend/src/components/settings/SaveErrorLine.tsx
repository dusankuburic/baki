import {useTranslation} from 'react-i18next'
import {useSettingsStore} from '@/stores/settingsStore'

/** The last refused save of the ai section, in the server's words (the field it names), until an ai save succeeds.
 * The change has been put back. A status, not an alert: the toast already announced it, and this line stays while
 * the panel is reopened. */
export default function SaveErrorLine() {
  const {t} = useTranslation('settings')
  const error = useSettingsStore(s => (s.saveError?.sections.includes('ai') ? s.saveError : null))
  if (!error) return null
  return (
    <p role="status" className="text-xs text-red-400">
      {error.message ? t('behavior.saveFailed', {message: error.message}) : t('behavior.saveFailedNoDetail')}
    </p>
  )
}
