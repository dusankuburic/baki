import {useTranslation} from 'react-i18next'
import React, {useState} from 'react'
import {AlertTriangle} from 'lucide-react'
import {authApi} from '@/api/auth'
import {useAuthStore} from '@/stores/authStore'
import {Button, Input} from '@/components/shared'
import {SectionCard} from './SectionCard'
import {setPostLogoutNotice} from './authNotice'

// DangerZoneSection is the self-service account-erasure flow. The
// confirmation form stays folded behind "Delete account…" so the destructive
// control is never one stray click away, and the delete button only arms
// once the typed email matches — the same check the server makes.
export const DangerZoneSection: React.FC = () => {
  const {t} = useTranslation('auth')
  const user = useAuthStore(s => s.user)
  const logout = useAuthStore(s => s.logout)
  const [open, setOpen] = useState(false)
  const [confirmEmail, setConfirmEmail] = useState('')
  const [deleting, setDeleting] = useState(false)
  const [deleteErr, setDeleteErr] = useState<string | null>(null)

  if (!user) return null
  const email = user.email
  const emailMatches = confirmEmail.trim().toLowerCase() === email.toLowerCase()

  const cancel = () => {
    setOpen(false)
    setConfirmEmail('')
    setDeleteErr(null)
  }

  async function handleDelete(e: React.FormEvent) {
    e.preventDefault()
    if (!emailMatches || deleting) return
    setDeleteErr(null)
    setDeleting(true)
    try {
      await authApi.deleteAccount(confirmEmail.trim())
      // Account erased server-side; tear down the local session. The auth
      // gate then shows the sign-in screen, with the confirmation on it.
      setPostLogoutNotice('account-deleted')
      await logout()
    } catch (err) {
      setDeleting(false)
      setDeleteErr(err instanceof Error ? err.message : t('account.deleteFailed'))
    }
  }

  return (
    <SectionCard
      id="profile-danger"
      icon={AlertTriangle}
      tone="danger"
      title={t('account.dangerZone')}
      description={t('account.dangerBody')}
    >
      {!open ? (
        <Button
          variant="danger"
          size="sm"
          onClick={() => setOpen(true)}
          aria-expanded={false}
          aria-controls="profile-delete-form"
        >
          {t('account.deleteStart')}
        </Button>
      ) : (
        <form id="profile-delete-form" onSubmit={e => void handleDelete(e)} className="space-y-3 max-w-md">
          <label htmlFor="profile-delete-email" className="block text-sm text-text-secondary">
            {t('account.confirmEmailLabel', {email})}
          </label>
          <Input
            id="profile-delete-email"
            type="email"
            value={confirmEmail}
            onChange={e => setConfirmEmail(e.target.value)}
            placeholder={t('account.confirmEmailPlaceholder')}
            autoComplete="off"
            spellCheck={false}
            autoFocus
          />
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" variant="danger" size="sm" disabled={!emailMatches} loading={deleting}>
              {t('account.deleteButton')}
            </Button>
            <Button type="button" variant="ghost" size="sm" onClick={cancel} disabled={deleting}>
              {t('account.cancel')}
            </Button>
          </div>
          {deleteErr && (
            <p role="alert" className="text-xs text-semantic-error">
              {deleteErr}
            </p>
          )}
        </form>
      )}
    </SectionCard>
  )
}
