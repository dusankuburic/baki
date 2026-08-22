import React, {useState} from 'react'
import {Download, AlertTriangle} from 'lucide-react'
import {authApi} from '@/api/auth'
import {useAuthStore} from '@/stores/authStore'
import {logger} from '@/lib/logger'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'
import {useToast, useConfirm} from '@/components/shared'

// AccountDataCard holds the profile-page account-management actions:
// exporting a data-subject bundle and the
// self-service account-erasure flow. Kept on the profile page so account
// management lives in one place instead of split across two panes.
export const AccountDataCard: React.FC = () => {
  const user = useAuthStore(s => s.user)
  const logout = useAuthStore(s => s.logout)
  const toast = useToast()
  const {confirm} = useConfirm()
  const [exporting, setExporting] = useState(false)

  const [confirmEmail, setConfirmEmail] = useState('')
  const [deleting, setDeleting] = useState(false)
  const [deleteErr, setDeleteErr] = useState<string | null>(null)

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
    } catch (err) {
      logger.warn('data export failed', err)
      toast.error('Export failed', {description: err instanceof Error ? err.message : String(err)})
    } finally {
      setExporting(false)
    }
  }

  async function handleDelete() {
    setDeleteErr(null)
    if (!user) return
    if (confirmEmail.trim().toLowerCase() !== user.email.toLowerCase()) {
      setDeleteErr('Type your email exactly as shown to confirm.')
      return
    }
    const ok = await confirm({
      title: 'Delete account',
      message: 'This permanently erases your account and your flows. This cannot be undone. Continue?',
      danger: true,
      confirmLabel: 'Delete account',
    })
    if (!ok) return
    setDeleting(true)
    try {
      await authApi.deleteAccount(confirmEmail.trim())
      // Account erased server-side; tear down local session. The auth gate
      // then redirects to the login screen.
      await logout()
    } catch (err) {
      setDeleting(false)
      setDeleteErr(err instanceof Error ? err.message : 'Deletion failed')
    }
  }

  return (
    <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
      <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
        <Download size={13} className="text-text-tertiary" />
        <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Account Data</h2>
      </div>

      <div className="p-5 space-y-4">
        <div>
          <span className="text-sm font-medium text-text-primary">Download your data</span>
          <p className="text-xs text-text-tertiary mt-0.5 mb-3">
            Export a copy of your account profile, flows, settings, API tokens, and audit history (data-subject access /
            portability).
          </p>
          <Button variant="secondary" size="sm" onClick={handleExport} loading={exporting}>
            Download my data
          </Button>
        </div>

        <div className="pt-4 border-t border-border-subtle">
          <span className="text-sm font-medium text-semantic-error flex items-center gap-1.5">
            <AlertTriangle size={14} />
            Danger zone
          </span>
          <p className="text-xs text-text-tertiary mt-1 mb-3">
            Permanently erase your account and all of your flows. Personal data is removed from shared records; security
            audit trails are retained but anonymized. This cannot be undone.
          </p>
          <label className="block text-xs text-text-secondary mb-1.5">
            Type your email (<span className="select-all">{user.email}</span>) to confirm:
          </label>
          <Input
            type="email"
            value={confirmEmail}
            onChange={e => setConfirmEmail(e.target.value)}
            placeholder={user.email}
            autoComplete="off"
            className="mb-3"
          />
          <Button variant="danger" size="sm" onClick={handleDelete} loading={deleting} disabled={!confirmEmail.trim()}>
            Permanently delete my account
          </Button>
          {deleteErr && <p className="text-xs text-semantic-error mt-2">{deleteErr}</p>}
        </div>
      </div>
    </div>
  )
}
