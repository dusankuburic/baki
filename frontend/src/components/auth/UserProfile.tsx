import React, { useState, useEffect } from 'react'
import { KeyRound, CheckCircle2, XCircle, User as UserIcon, Building2, Monitor, Trash2 } from 'lucide-react'
import clsx from 'clsx'
import { useAuthStore } from '@/stores/authStore'
import { useOrgStore } from '@/stores/orgStore'
import { authApi, type SessionInfo } from '@/api/auth'
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

function formatDate(value: string) {
  try {
    return new Date(value).toLocaleString()
  } catch {
    return value
  }
}

export const UserProfile: React.FC = () => {
  const { user, logout, updateUser } = useAuthStore()
  const { organisations, loadOrgs } = useOrgStore()

  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword]         = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [status, setStatus] = useState<{ type: 'success' | 'error'; message: string } | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)

  // --- Display name editing ---
  const [isEditingProfile, setIsEditingProfile] = useState(false)
  const [displayNameInput, setDisplayNameInput] = useState(user?.displayName ?? '')
  const [profileStatus, setProfileStatus] = useState<{ type: 'success' | 'error'; message: string } | null>(null)
  const [isSavingProfile, setIsSavingProfile] = useState(false)

  // --- Sessions ---
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [sessionsLoading, setSessionsLoading] = useState(true)
  const [sessionsError, setSessionsError] = useState<string | null>(null)
  const [revokingId, setRevokingId] = useState<string | null>(null)

  useEffect(() => {
    loadOrgs()
  }, [loadOrgs])

  useEffect(() => {
    let cancelled = false
    authApi.listSessions()
      .then(list => { if (!cancelled) setSessions(list ?? []) })
      .catch(err => { if (!cancelled) setSessionsError(err instanceof Error ? err.message : 'Failed to load sessions') })
      .finally(() => { if (!cancelled) setSessionsLoading(false) })
    return () => { cancelled = true }
  }, [])

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

  const handleSaveProfile = async (e: React.FormEvent) => {
    e.preventDefault()
    setIsSavingProfile(true)
    setProfileStatus(null)
    try {
      const updated = await authApi.updateProfile({
        displayName: displayNameInput.trim(),
        avatarUrl: user?.avatarUrl ?? '',
      })
      updateUser({ displayName: updated.displayName, avatarUrl: updated.avatarUrl })
      setProfileStatus({ type: 'success', message: 'Profile updated' })
      setIsEditingProfile(false)
    } catch (err) {
      setProfileStatus({ type: 'error', message: err instanceof Error ? err.message : 'Failed to update profile' })
    } finally {
      setIsSavingProfile(false)
    }
  }

  const handleRevokeSession = async (id: string) => {
    setRevokingId(id)
    setSessionsError(null)
    try {
      await authApi.revokeSession(id)
      setSessions(prev => prev.filter(s => s.id !== id))
    } catch (err) {
      setSessionsError(err instanceof Error ? err.message : 'Failed to revoke session')
    } finally {
      setRevokingId(null)
    }
  }

  if (!user) return null

  const displayName = user.displayName?.trim()
  const initials = (displayName?.charAt(0) || user.email.charAt(0)).toUpperCase()

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
              <p className="text-sm font-medium text-text-primary">{displayName || user.email}</p>
              {displayName && (
                <p className="text-xs text-text-tertiary">{user.email}</p>
              )}
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

      {/* Profile */}
      <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
        <div className="px-5 py-3 border-b border-border-subtle flex items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <UserIcon size={13} className="text-text-tertiary" />
            <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Profile</h2>
          </div>
          {!isEditingProfile && (
            <button
              type="button"
              className="text-xs font-medium text-brand-400 hover:text-brand-300"
              onClick={() => {
                setDisplayNameInput(user.displayName ?? '')
                setProfileStatus(null)
                setIsEditingProfile(true)
              }}
            >
              Edit
            </button>
          )}
        </div>

        <div className="p-5 space-y-3">
          {isEditingProfile ? (
            <form onSubmit={handleSaveProfile} className="space-y-3">
              <div>
                <label className="text-xs font-medium text-text-secondary block mb-1.5">Display Name</label>
                <Input
                  type="text"
                  value={displayNameInput}
                  onChange={e => setDisplayNameInput(e.target.value)}
                  placeholder="Enter a display name"
                  maxLength={100}
                />
              </div>

              {profileStatus && (
                <div className={clsx(
                  'flex items-start gap-2 rounded-lg px-3 py-2.5 text-sm border',
                  profileStatus.type === 'success'
                    ? 'bg-semantic-success/10 border-semantic-success/30 text-semantic-success'
                    : 'bg-semantic-error/10 border-semantic-error/30 text-semantic-error'
                )}>
                  {profileStatus.type === 'success'
                    ? <CheckCircle2 size={14} className="mt-0.5 flex-shrink-0" />
                    : <XCircle size={14} className="mt-0.5 flex-shrink-0" />}
                  <span>{profileStatus.message}</span>
                </div>
              )}

              <div className="flex gap-2">
                <Button type="submit" variant="primary" size="md" loading={isSavingProfile}>
                  Save
                </Button>
                <Button
                  type="button"
                  variant="secondary"
                  size="md"
                  onClick={() => setIsEditingProfile(false)}
                  disabled={isSavingProfile}
                >
                  Cancel
                </Button>
              </div>
            </form>
          ) : (
            <div>
              <label className="text-xs font-medium text-text-secondary block mb-1">Display Name</label>
              <p className="text-sm text-text-primary">{displayName || <span className="text-text-tertiary">Not set</span>}</p>
              {profileStatus && profileStatus.type === 'success' && (
                <div className="flex items-start gap-2 rounded-lg px-3 py-2.5 mt-3 text-sm border bg-semantic-success/10 border-semantic-success/30 text-semantic-success">
                  <CheckCircle2 size={14} className="mt-0.5 flex-shrink-0" />
                  <span>{profileStatus.message}</span>
                </div>
              )}
            </div>
          )}
        </div>
      </div>

      {/* Organisations */}
      <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
        <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
          <Building2 size={13} className="text-text-tertiary" />
          <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Organisations</h2>
        </div>

        <div className="p-5">
          {organisations.length === 0 ? (
            <p className="text-sm text-text-tertiary">You're not a member of any organisations.</p>
          ) : (
            <ul className="space-y-2">
              {organisations.map(org => {
                const membership = org.members.find(m => m.userId === user.id)
                return (
                  <li key={org.id} className="flex items-center justify-between gap-3 rounded-lg bg-surface-3 px-3 py-2.5">
                    <span className="text-sm text-text-primary truncate">{org.name}</span>
                    {membership && (
                      <span className={clsx(
                        'px-2 py-0.5 rounded-md text-xs font-semibold uppercase flex-shrink-0',
                        roleBadgeClass(membership.role)
                      )}>
                        {membership.role}
                      </span>
                    )}
                  </li>
                )
              })}
            </ul>
          )}
        </div>
      </div>

      {/* Active Sessions */}
      <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
        <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
          <Monitor size={13} className="text-text-tertiary" />
          <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Active Sessions</h2>
        </div>

        <div className="p-5 space-y-2">
          {sessionsError && (
            <div className="flex items-start gap-2 rounded-lg px-3 py-2.5 text-sm border bg-semantic-error/10 border-semantic-error/30 text-semantic-error">
              <XCircle size={14} className="mt-0.5 flex-shrink-0" />
              <span>{sessionsError}</span>
            </div>
          )}

          {sessionsLoading ? (
            <p className="text-sm text-text-tertiary">Loading sessions...</p>
          ) : sessions.length === 0 ? (
            <p className="text-sm text-text-tertiary">No active sessions found.</p>
          ) : (
            <ul className="space-y-2">
              {sessions.map(session => (
                <li key={session.id} className="flex items-center justify-between gap-3 rounded-lg bg-surface-3 px-3 py-2.5">
                  <div className="min-w-0">
                    <p className="text-sm text-text-primary">Signed in {formatDate(session.createdAt)}</p>
                    <p className="text-xs text-text-tertiary">Expires {formatDate(session.expiresAt)}</p>
                  </div>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => handleRevokeSession(session.id)}
                    loading={revokingId === session.id}
                    title="Revoke session"
                  >
                    <Trash2 size={14} />
                  </Button>
                </li>
              ))}
            </ul>
          )}
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
