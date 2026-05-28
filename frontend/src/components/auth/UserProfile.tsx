import React, { useState } from 'react'
import { KeyRound, CheckCircle2, XCircle } from 'lucide-react'
import clsx from 'clsx'
import { useAuthStore } from '@/stores/authStore'
import { authApi } from '@/api/auth'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'

function roleBadgeClass(role: string) {
  switch (role) {
    case 'admin':   return 'bg-block-subflow/10 text-block-subflow'
    case 'member':  return 'bg-block-action/10 text-block-action'
    case 'viewer':  return 'bg-block-condition/10 text-block-condition'
    default:        return 'bg-surface-4 text-text-tertiary'
  }
}

export const UserProfile: React.FC = () => {
  const { user, logout } = useAuthStore()
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword]         = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [status, setStatus] = useState<{ type: 'success' | 'error'; message: string } | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)

  const handleChangePassword = async (e: React.FormEvent) => {
    e.preventDefault()
    if (newPassword !== confirmPassword) {
      setStatus({ type: 'error', message: 'New passwords do not match' })
      return
    }
    setIsSubmitting(true)
    setStatus(null)
    try {
      await authApi.changePassword(currentPassword, newPassword)
      setStatus({ type: 'success', message: 'Password updated successfully' })
      setCurrentPassword('')
      setNewPassword('')
      setConfirmPassword('')
    } catch (err) {
      setStatus({ type: 'error', message: err instanceof Error ? err.message : 'Failed to update password' })
    } finally {
      setIsSubmitting(false)
    }
  }

  if (!user) return null

  const initials = user.email.charAt(0).toUpperCase()

  return (
    <div className="p-6 md:p-8 max-w-lg mx-auto space-y-5">

      {/* User Info Card */}
      <div className="bg-surface-2 border border-border-default rounded-xl p-5">
        <div className="flex items-center justify-between gap-4">
          <div className="flex items-center gap-3">
            <div className="w-11 h-11 rounded-full bg-brand-500/20 flex items-center justify-center text-brand-400 font-bold text-lg flex-shrink-0 select-none">
              {initials}
            </div>
            <div>
              <p className="text-sm font-medium text-text-primary">{user.email}</p>
              <span className={clsx(
                'inline-block mt-1 px-2 py-0.5 rounded-md text-xs font-semibold uppercase',
                roleBadgeClass(user.role)
              )}>
                {user.role}
              </span>
            </div>
          </div>
          <Button variant="danger" size="sm" onClick={() => logout()}>
            Logout
          </Button>
        </div>
      </div>

      {/* Change Password */}
      <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
        <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
          <KeyRound size={13} className="text-text-tertiary" />
          <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Change Password</h2>
        </div>

        <form onSubmit={handleChangePassword} className="p-5 space-y-3">
          <div>
            <label className="text-xs font-medium text-text-secondary block mb-1.5">Current Password</label>
            <Input
              type="password"
              value={currentPassword}
              onChange={e => setCurrentPassword(e.target.value)}
              placeholder="Enter current password"
              required
            />
          </div>
          <div>
            <label className="text-xs font-medium text-text-secondary block mb-1.5">New Password</label>
            <Input
              type="password"
              value={newPassword}
              onChange={e => setNewPassword(e.target.value)}
              placeholder="Enter new password"
              required
            />
          </div>
          <div>
            <label className="text-xs font-medium text-text-secondary block mb-1.5">Confirm New Password</label>
            <Input
              type="password"
              value={confirmPassword}
              onChange={e => setConfirmPassword(e.target.value)}
              placeholder="Confirm new password"
              required
            />
          </div>

          {status && (
            <div className={clsx(
              'flex items-start gap-2 rounded-lg px-3 py-2.5 text-sm border',
              status.type === 'success'
                ? 'bg-semantic-success/10 border-semantic-success/30 text-semantic-success'
                : 'bg-semantic-error/10 border-semantic-error/30 text-semantic-error'
            )}>
              {status.type === 'success'
                ? <CheckCircle2 size={14} className="mt-0.5 flex-shrink-0" />
                : <XCircle size={14} className="mt-0.5 flex-shrink-0" />}
              <span>{status.message}</span>
            </div>
          )}

          <Button
            type="submit"
            variant="primary"
            size="md"
            fullWidth
            loading={isSubmitting}
          >
            Update Password
          </Button>
        </form>
      </div>
    </div>
  )
}
