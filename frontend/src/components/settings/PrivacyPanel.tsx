// Downloading account data and deleting the account live on the Profile page
// (AccountDataCard) now, so account management isn't split across two panes.
// This panel stays purely informational.
import {useTranslation} from 'react-i18next'
import {useSystemStore} from '@/stores/systemStore'
import {isTauri} from '@/platform/guards'

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
    <div>
      <h2 className="text-xl font-semibold text-text-primary">Privacy</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">{t('privacy.subtitle')}</p>

      <div className="space-y-4">
        <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
          <span className="text-sm font-medium text-text-primary">{t('privacy.apiKeysTitle')}</span>
          <p className="text-xs text-text-tertiary mt-0.5">
            {localBackend ? t('privacy.apiKeysLocal') : t('privacy.apiKeysServer')}
          </p>
        </div>

        <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
          <span className="text-sm font-medium text-text-primary">{t('privacy.aiDataTitle')}</span>
          <p className="text-xs text-text-tertiary mt-0.5">
            Before any flow content is sent to an AI provider, known credential fields, secret-shaped patterns (tokens,
            connection strings), and high-entropy strings are automatically redacted.
          </p>
        </div>
      </div>
    </div>
  )
}
