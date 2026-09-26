// Downloading account data and deleting the account live on the Profile page
// (AccountDataCard) now, so account management isn't split across two panes.
// This panel stays purely informational.
import {useTranslation} from 'react-i18next'
import {useSystemStore} from '@/stores/systemStore'
import {isTauri} from '@/platform/guards'
import SettingRow from './SettingRow'

export default function PrivacyPanel() {
  const {t} = useTranslation('settings')
  // Where keys live depends on the backend, not the shell: a local backend
  // keeps them in the OS keychain, a server backend receives them and stores
  // them encrypted. Session analytics is the local-backend capability (same
  // signal AIBehaviorPanel uses); isTauri() only covers the moment before
  // system info has loaded.
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
