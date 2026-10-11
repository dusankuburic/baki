import {useTranslation} from 'react-i18next'
import React from 'react'
import {KeyRound} from 'lucide-react'
import {useUIStore} from '@/stores/uiStore'
import {Button} from '@/components/shared'
import {SectionCard} from './SectionCard'

export const ApiTokensSection: React.FC = () => {
  const {t} = useTranslation('auth')
  const openSettings = useUIStore(s => s.openSettings)
  const openTokens = () => openSettings('tokens')
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
