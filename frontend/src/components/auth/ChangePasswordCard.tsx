import React, {useState, useCallback} from 'react'
import {KeyRound} from 'lucide-react'
import {authApi} from '@/api/auth'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'
import {useToast} from '@/components/shared'

export const ChangePasswordCard: React.FC = () => {
  const toast = useToast()
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [isSubmitting, setIsSubmitting] = useState(false)

  const handleSubmit = useCallback(async (e: React.FormEvent) => {
    e.preventDefault()
    if (newPassword !== confirmPassword) {
      toast.error('New passwords do not match')
      return
    }
    setIsSubmitting(true)
    try {
      await authApi.changePassword(currentPassword, newPassword)
      toast.success('Password updated successfully')
      setCurrentPassword('')
      setNewPassword('')
      setConfirmPassword('')
    } catch (err) {
      toast.error('Failed to update password', {description: err instanceof Error ? err.message : String(err)})
    } finally {
      setIsSubmitting(false)
    }
  }, [currentPassword, newPassword, confirmPassword, toast])

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

        <Button type="submit" variant="primary" size="md" fullWidth loading={isSubmitting}>
          Update Password
        </Button>
      </form>
    </div>
  )
}
