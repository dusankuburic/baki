import {getErrorMessage} from '@/lib/getErrorMessage'
import {triggerDownload} from '@/lib/download'
import {useTranslation} from 'react-i18next'
import React, {useState} from 'react'
import {Download} from 'lucide-react'
import {authApi} from '@/api/auth'
import {useAuthStore} from '@/stores/authStore'
import {logger} from '@/lib/logger'
import {Button, useToast} from '@/components/shared'
import {SectionCard} from './SectionCard'

const INCLUDED = ['profile', 'flows', 'findings', 'chat', 'audit', 'tokens', 'orgs'] as const

export const AccountDataSection: React.FC = () => {
  const {t} = useTranslation('auth')
  const user = useAuthStore(s => s.user)
  const toast = useToast()
  const [exporting, setExporting] = useState(false)

  if (!user) return null

  async function handleExport() {
    setExporting(true)
    try {
      const blob = await authApi.exportAccount()
      triggerDownload(blob, `baki-account-export-${user?.id ?? 'data'}.json`)
      toast.success(t('account.exportDone'))
    } catch (err) {
      logger.warn('data export failed', err)
      toast.error(t('account.exportFailed'), {description: getErrorMessage(err)})
    } finally {
      setExporting(false)
    }
  }

  return (
    <SectionCard
      id="profile-export"
      icon={Download}
      title={t('account.heading')}
      description={t('account.downloadBody')}
      action={
        <Button variant="secondary" size="sm" icon={Download} onClick={() => void handleExport()} loading={exporting}>
          {t('account.downloadButton')}
        </Button>
      }
    >
      <p className="text-xs font-medium text-text-secondary mb-2">{t('account.includesLabel')}</p>
      <ul className="flex flex-wrap gap-1.5">
        {INCLUDED.map(key => (
          <li key={key} className="px-2 py-0.5 rounded-md bg-surface-3 text-xs text-text-secondary">
            {t(`accountInclude.${key}`)}
          </li>
        ))}
      </ul>
    </SectionCard>
  )
}
