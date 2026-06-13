import React, {useState, useCallback} from 'react'
import {KeyRound, CheckCircle2, XCircle} from 'lucide-react'
import clsx from 'clsx'
import {authApi} from '@/api/auth'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'

export const ChangePasswordCard: React.FC = () => {
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [status, setStatus] = useState<{ type: 'success' | 'error'; message: string } | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)

  const handleSubmit = useCallback(async (e: React.FormEvent) => {
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
  }, [currentPassword, newPassword, confirmPassword])

  return (
    <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
      <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
        <KeyRound size={13} className="text-text-tertiary" />
        <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Change Password</h2>
      </div>

      <form onSubmit={handleSubmit} className="p-5 space-y-3">
        <div>
          <label className="text-xs font-medium text-text-secondary block mb-1.5">Current Password</label>
          <Input type="password" value={currentPassword} onChange={e => setCurrentPassword(e.target.value)} placeholder="Enter current password" required />
        </div>
        <div>
          <label className="text-xs font-medium text-text-secondary block mb-1.5">New Password</label>
          <Input type="password" value={newPassword} onChange={e => setNewPassword(e.target.value)} placeholder="Enter new password" required />
        </div>
        <div>
          <label className="text-xs font-medium text-text-secondary block mb-1.5">Confirm New Password</label>
          <Input type="password" value={confirmPassword} onChange={e => setConfirmPassword(e.target.value)} placeholder="Confirm new password" required />
        </div>

        {status && (
          <div className={clsx(
            'flex items-start gap-2 rounded-lg px-3 py-2.5 text-sm border',
            status.type === 'success' ? 'bg-semantic-success/10 border-semantic-success/30 text-semantic-success' : 'bg-semantic-error/10 border-semantic-error/30 text-semantic-error'
          )}>
            {status.type === 'success' ? <CheckCircle2 size={14} className="mt-0.5 flex-shrink-0" /> : <XCircle size={14} className="mt-0.5 flex-shrink-0" />}
            <span>{status.message}</span>
          </div>
        )}

        <Button type="submit" variant="primary" size="md" fullWidth loading={isSubmitting}>
          Update Password
        </Button>
      </form>
    </div>
  )
}
