import {useTranslation} from 'react-i18next'
import {useSystemStore} from '@/stores/systemStore'
import {isTauri} from '@/platform/guards'
import SettingRow from './SettingRow'

export default function PrivacyPanel() {
  const {t} = useTranslation('settings')
  const systemInfo = useSystemStore(s => s.info)
  const localBackend = systemInfo ? systemInfo.capabilities.sessionAnalytics : isTauri()
  return (
    <div className="space-y-4">
      <SettingRow
        title={t('privacy.apiKeysTitle')}
        hint={localBackend ? t('privacy.apiKeysLocal') : t('privacy.apiKeysServer')}
      />
      <SettingRow title={t('privacy.aiDataTitle')} hint={t('privacy.aiDataBody')} />
    </div>
  )
}
