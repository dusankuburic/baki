import {useTranslation} from 'react-i18next'
import React from 'react'
import {KeyRound} from 'lucide-react'
import {useUIStore} from '@/stores/uiStore'
import {Button} from '@/components/shared'
import {SectionCard} from './SectionCard'

// ApiTokensSection points at Settings → API tokens: users look for personal
// access tokens on their profile, and the panel that manages them is a
// modal, so the profile links there instead of hosting a second copy.
export const ApiTokensSection: React.FC = () => {
  const {t} = useTranslation('auth')
  const setSettingsSection = useUIStore(s => s.setSettingsSection)
  const setSettingsOpen = useUIStore(s => s.setSettingsOpen)
  const openTokens = () => {
    setSettingsSection('tokens')
    setSettingsOpen(true)
  }
  return (
    <SectionCard
      id="profile-tokens"
      icon={KeyRound}
      title={t('account.tokensHeading')}
      description={t('account.tokensBody')}
      action={
        <Button variant="secondary" size="sm" icon={KeyRound} onClick={openTokens}>
          {t('account.tokensManage')}
        </Button>
      }
    />
  )
}
