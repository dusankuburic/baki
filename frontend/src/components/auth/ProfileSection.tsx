import {getErrorMessage} from '@/lib/getErrorMessage'
import {logger} from '@/lib/logger'
import {useTranslation} from 'react-i18next'
import React, {useState, useCallback} from 'react'
import {Check, Copy, Pencil, User as UserIcon} from 'lucide-react'
import {useAuthStore} from '@/stores/authStore'
import {authApi} from '@/api/auth'
import {Avatar, Button, IconButton, Input, useToast} from '@/components/shared'
import {useCopy} from '@/hooks/useCopy'
import {SectionCard} from './SectionCard'

// isHttpUrl mirrors the server's avatar check (a parseable http(s) URL) so a
// typo is caught in the form instead of coming back as a 400.
function isHttpUrl(value: string): boolean {
  try {
    const url = new URL(value)
    return url.protocol === 'https:' || url.protocol === 'http:'
  } catch (err) {
    logger.warn('ProfileSection: avatar URL did not parse as http(s)', err)
    return false
  }
}

// ProfileSection shows the editable identity fields. Editing state belongs to
// the page: the hero's "Edit profile" opens the same form.
export const ProfileSection: React.FC<{editing: boolean; onEditingChange: (editing: boolean) => void}> = ({
  editing,
  onEditingChange,
}) => {
  const {t} = useTranslation('auth')
  const user = useAuthStore(s => s.user)
  const updateUser = useAuthStore(s => s.updateUser)
  const toast = useToast()
  const {copied, copy} = useCopy()
  const [displayNameInput, setDisplayNameInput] = useState(user?.displayName ?? '')
  const [avatarUrlInput, setAvatarUrlInput] = useState(user?.avatarUrl ?? '')
  const [isSaving, setIsSaving] = useState(false)

  // The draft is re-seeded from the store whenever editing starts, whichever
  // control started it. Adjusted during render so no stale-draft frame paints.
  const [wasEditing, setWasEditing] = useState(editing)
  if (editing !== wasEditing) {
    setWasEditing(editing)
    if (editing) {
      setDisplayNameInput(user?.displayName ?? '')
      setAvatarUrlInput(user?.avatarUrl ?? '')
    }
  }

  const nextName = displayNameInput.trim()
  const nextAvatar = avatarUrlInput.trim()
  const avatarError = nextAvatar && !isHttpUrl(nextAvatar) ? t('profile.avatarInvalid') : undefined
  const dirty = nextName !== (user?.displayName ?? '') || nextAvatar !== (user?.avatarUrl ?? '')

  const handleSave = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      if (!dirty || avatarError) return
      setIsSaving(true)
      try {
        const updated = await authApi.updateProfile({displayName: nextName, avatarUrl: nextAvatar})
        updateUser({displayName: updated.displayName, avatarUrl: updated.avatarUrl})
        toast.success(t('profile.updated'))
        onEditingChange(false)
      } catch (err) {
        toast.error(t('profile.updateFailed'), {description: getErrorMessage(err)})
      } finally {
        setIsSaving(false)
      }
    },
    [dirty, avatarError, nextName, nextAvatar, updateUser, toast, t, onEditingChange],
  )

  if (!user) return null
  const displayName = user.displayName?.trim()

  return (
    <SectionCard
      id="profile-identity"
      icon={UserIcon}
      title={t('profile.heading')}
      description={t('profile.description')}
      action={
        !editing ? (
          <Button variant="ghost" size="sm" icon={Pencil} onClick={() => onEditingChange(true)}>
            {t('profile.edit')}
          </Button>
        ) : undefined
      }
    >
      {editing ? (
        <form
          onSubmit={e => void handleSave(e)}
          className="space-y-4"
          onKeyDown={e => {
            if (e.key === 'Escape') {
              e.preventDefault()
              onEditingChange(false)
            }
          }}
        >
          <div className="flex items-start gap-4">
            <Avatar name={nextName || user.email} colorSeed={user.id} avatarUrl={nextAvatar || undefined} size="xl" />
            <div className="flex-1 min-w-0 space-y-3">
              <div>
                <label htmlFor="profile-display-name" className="text-xs font-medium text-text-secondary block mb-1.5">
                  {t('profile.displayName')}
                </label>
                <Input
                  id="profile-display-name"
                  type="text"
                  value={displayNameInput}
                  onChange={e => setDisplayNameInput(e.target.value)}
                  placeholder={t('profile.displayNamePlaceholder')}
                  maxLength={100}
                  autoFocus
                />
              </div>
              <div>
                <label htmlFor="profile-avatar-url" className="text-xs font-medium text-text-secondary block mb-1.5">
                  {t('profile.avatarUrl')}
                </label>
                <Input
                  id="profile-avatar-url"
                  type="url"
                  value={avatarUrlInput}
                  onChange={e => setAvatarUrlInput(e.target.value)}
                  placeholder={t('profile.avatarPlaceholder')}
                  maxLength={2048}
                  error={avatarError}
                  hint={avatarError ? undefined : t('profile.avatarHint')}
                />
              </div>
            </div>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" variant="primary" size="md" loading={isSaving} disabled={!dirty || !!avatarError}>
              {t('profile.save')}
            </Button>
            <Button
              type="button"
              variant="secondary"
              size="md"
              onClick={() => onEditingChange(false)}
              disabled={isSaving}
            >
              {t('profile.cancel')}
            </Button>
          </div>
        </form>
      ) : (
        <dl className="space-y-3">
          <Field label={t('profile.displayName')}>
            {displayName || <span className="text-text-tertiary">{t('profile.notSet')}</span>}
          </Field>
          <Field label={t('profile.email')}>
            <span className="inline-flex items-center gap-1 max-w-full">
              <span className="truncate">{user.email}</span>
              <IconButton
                icon={copied ? Check : Copy}
                size="sm"
                label={copied ? t('profile.copied') : t('profile.copyEmail')}
                onClick={() => copy(user.email)}
              />
            </span>
          </Field>
          <Field label={t('profile.avatarUrl')}>
            {user.avatarUrl ? (
              <span className="block truncate" title={user.avatarUrl}>
                {user.avatarUrl}
              </span>
            ) : (
              <span className="text-text-tertiary">{t('profile.notSet')}</span>
            )}
          </Field>
        </dl>
      )}
    </SectionCard>
  )
}

function Field({label, children}: {label: string; children: React.ReactNode}) {
  return (
    <div className="profile-field">
      <dt className="text-xs font-medium text-text-secondary">{label}</dt>
      <dd className="text-sm text-text-primary min-w-0">{children}</dd>
    </div>
  )
}
