import {useTranslation} from 'react-i18next'
import React, {useState} from 'react'
import {Download} from 'lucide-react'
import {authApi} from '@/api/auth'
import {useAuthStore} from '@/stores/authStore'
import {logger} from '@/lib/logger'
import {Button, useToast} from '@/components/shared'
import {SectionCard} from './SectionCard'

// What the bundle holds, as chips instead of a sixty-word sentence. Mirrors
// the sections of UserDataExport in internal/storage/interfaces/storage.go.
const INCLUDED = ['profile', 'flows', 'findings', 'chat', 'audit', 'tokens', 'orgs'] as const

// AccountDataSection is the data-subject access / portability export.
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
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `baki-account-export-${user?.id ?? 'data'}.json`
      document.body.appendChild(a)
      a.click()
      a.remove()
      URL.revokeObjectURL(url)
      toast.success(t('account.exportDone'))
    } catch (err) {
      logger.warn('data export failed', err)
      toast.error(t('account.exportFailed'), {description: err instanceof Error ? err.message : String(err)})
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
