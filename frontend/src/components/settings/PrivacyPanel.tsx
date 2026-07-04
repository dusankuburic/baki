import {useState} from 'react'
import Button from '@/components/shared/Button'
import {authApi} from '@/api/auth'
import {useAuthStore} from '@/stores/authStore'
import {logger} from '@/lib/logger'

export default function PrivacyPanel() {
  const {user, logout} = useAuthStore()
  const [exporting, setExporting] = useState(false)
  const [exportErr, setExportErr] = useState<string | null>(null)

  const [confirmEmail, setConfirmEmail] = useState('')
  const [deleting, setDeleting] = useState(false)
  const [deleteErr, setDeleteErr] = useState<string | null>(null)

  // Account erasure / export are cloud (JWT) features backed by a user account.
  const cloudEnabled = !!user

  async function handleExport() {
    setExporting(true)
    setExportErr(null)
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
      setExportErr(err instanceof Error ? err.message : 'Export failed')
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
    if (!window.confirm('This permanently erases your account and your flows. This cannot be undone. Continue?')) {
      return
    }
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
    <div>
      <h2 className="text-xl font-semibold text-text-primary">Privacy</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">
        Control how PAD Analyzer handles your data.
      </p>

      <div className="space-y-4">
        <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
          <span className="text-sm font-medium text-text-primary">API Keys</span>
          <p className="text-xs text-text-tertiary mt-0.5">
            All API keys are stored securely in your operating system's keychain.
            They are never sent to any server other than the respective AI provider.
          </p>
        </div>

        <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
          <span className="text-sm font-medium text-text-primary">Data sent to AI providers</span>
          <p className="text-xs text-text-tertiary mt-0.5">
            Before any flow content is sent to an AI provider, known credential fields,
            secret-shaped patterns (tokens, connection strings), and high-entropy strings
            are automatically redacted.
          </p>
        </div>

        {cloudEnabled && (
          <>
            <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
              <span className="text-sm font-medium text-text-primary">Download your data</span>
              <p className="text-xs text-text-tertiary mt-0.5 mb-3">
                Export a copy of your account profile, flows, settings, API tokens, and audit
                history (data-subject access / portability).
              </p>
              <Button variant="secondary" size="sm" onClick={handleExport} disabled={exporting}>
                {exporting ? 'Preparing…' : 'Download my data'}
              </Button>
              {exportErr && <p className="text-xs text-finding-error mt-2">{exportErr}</p>}
            </div>

            <div className="py-3 px-4 rounded-lg bg-finding-error/5 border border-finding-error/30">
              <span className="text-sm font-medium text-finding-error">Delete account</span>
              <p className="text-xs text-text-tertiary mt-0.5 mb-3">
                Permanently erase your account and all of your flows. Personal data is removed from
                shared records; security audit trails are retained but anonymized. This cannot be undone.
              </p>
              <label className="block text-xs text-text-secondary mb-1">
                Type your email (<span className="select-all">{user.email}</span>) to confirm:
              </label>
              <input
                type="email"
                value={confirmEmail}
                onChange={(e) => setConfirmEmail(e.target.value)}
                placeholder={user.email}
                autoComplete="off"
                className="w-full mb-3 px-3 py-2 text-sm rounded-md bg-surface-3 border border-border-default text-text-primary focus:outline-none focus:border-brand-500"
              />
              <Button
                variant="danger"
                size="sm"
                onClick={handleDelete}
                disabled={deleting || !confirmEmail.trim()}
              >
                {deleting ? 'Erasing…' : 'Permanently delete my account'}
              </Button>
              {deleteErr && <p className="text-xs text-finding-error mt-2">{deleteErr}</p>}
            </div>
          </>
        )}
      </div>
    </div>
  )
}
