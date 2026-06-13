import React, {useState, useCallback} from 'react'
import {CheckCircle2, XCircle, User as UserIcon} from 'lucide-react'
import clsx from 'clsx'
import {useAuthStore} from '@/stores/authStore'
import {authApi} from '@/api/auth'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'

export const ProfileCard: React.FC = () => {
  const {user, updateUser} = useAuthStore()

  const [isEditing, setIsEditing] = useState(false)
  const [displayNameInput, setDisplayNameInput] = useState(user?.displayName ?? '')
  const [status, setStatus] = useState<{ type: 'success' | 'error'; message: string } | null>(null)
  const [isSaving, setIsSaving] = useState(false)

  const avatarUrl = user?.avatarUrl ?? ''

  const handleSave = useCallback(async (e: React.FormEvent) => {
    e.preventDefault()
    setIsSaving(true)
    setStatus(null)
    try {
      const updated = await authApi.updateProfile({
        displayName: displayNameInput.trim(),
        avatarUrl,
      })
      updateUser({ displayName: updated.displayName, avatarUrl: updated.avatarUrl })
      setStatus({ type: 'success', message: 'Profile updated' })
      setIsEditing(false)
    } catch (err) {
      setStatus({ type: 'error', message: err instanceof Error ? err.message : 'Failed to update profile' })
    } finally {
      setIsSaving(false)
    }
  }, [displayNameInput, avatarUrl, updateUser])

  const displayName = user?.displayName?.trim()

  return (
    <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
      <div className="px-5 py-3 border-b border-border-subtle flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <UserIcon size={13} className="text-text-tertiary" />
          <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Profile</h2>
        </div>
        {!isEditing && (
          <button
            type="button"
            className="text-xs font-medium text-brand-400 hover:text-brand-300"
            onClick={() => { setDisplayNameInput(user?.displayName ?? ''); setStatus(null); setIsEditing(true) }}
          >
            Edit
          </button>
        )}
      </div>

      <div className="p-5 space-y-3">
        {isEditing ? (
          <form onSubmit={handleSave} className="space-y-3">
            <div>
              <label className="text-xs font-medium text-text-secondary block mb-1.5">Display Name</label>
              <Input type="text" value={displayNameInput} onChange={e => setDisplayNameInput(e.target.value)} placeholder="Enter a display name" maxLength={100} />
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

            <div className="flex gap-2">
              <Button type="submit" variant="primary" size="md" loading={isSaving}>Save</Button>
              <Button type="button" variant="secondary" size="md" onClick={() => setIsEditing(false)} disabled={isSaving}>Cancel</Button>
            </div>
          </form>
        ) : (
          <div>
            <label className="text-xs font-medium text-text-secondary block mb-1">Display Name</label>
            <p className="text-sm text-text-primary">{displayName || <span className="text-text-tertiary">Not set</span>}</p>
            {status && status.type === 'success' && (
              <div className="flex items-start gap-2 rounded-lg px-3 py-2.5 mt-3 text-sm border bg-semantic-success/10 border-semantic-success/30 text-semantic-success">
                <CheckCircle2 size={14} className="mt-0.5 flex-shrink-0" />
                <span>{status.message}</span>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  )
}
