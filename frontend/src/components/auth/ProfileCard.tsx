import {useTranslation} from 'react-i18next'
import React, {useState, useCallback} from 'react'
import {User as UserIcon} from 'lucide-react'
import {useAuthStore} from '@/stores/authStore'
import {authApi} from '@/api/auth'
import Button from '@/components/shared/Button'
import Input from '@/components/shared/Input'
import Avatar from '@/components/shared/Avatar'
import {useToast} from '@/components/shared'

export const ProfileCard: React.FC = () => {
  const {t} = useTranslation('auth')
  const user = useAuthStore(s => s.user)
  const updateUser = useAuthStore(s => s.updateUser)
  const toast = useToast()

  const [isEditing, setIsEditing] = useState(false)
  const [displayNameInput, setDisplayNameInput] = useState(user?.displayName ?? '')
  const [avatarUrlInput, setAvatarUrlInput] = useState(user?.avatarUrl ?? '')
  const [isSaving, setIsSaving] = useState(false)

  const handleSave = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      setIsSaving(true)
      try {
        const updated = await authApi.updateProfile({
          displayName: displayNameInput.trim(),
          avatarUrl: avatarUrlInput.trim(),
        })
        updateUser({displayName: updated.displayName, avatarUrl: updated.avatarUrl})
        toast.success(t('profile.updated'))
        setIsEditing(false)
      } catch (err) {
        toast.error(t('profile.updateFailed'), {description: err instanceof Error ? err.message : String(err)})
      } finally {
        setIsSaving(false)
      }
    },
    [displayNameInput, avatarUrlInput, updateUser, toast, t],
  )

  const displayName = user?.displayName?.trim()

  return (
    <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
      <div className="px-5 py-3 border-b border-border-subtle flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <UserIcon size={13} className="text-text-tertiary" />
          <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">{t('profile.heading')}</h2>
        </div>
        {!isEditing && (
          <button
            type="button"
            className="text-xs font-medium text-brand-400 hover:text-brand-300"
            onClick={() => {
              setDisplayNameInput(user?.displayName ?? '')
              setAvatarUrlInput(user?.avatarUrl ?? '')
              setIsEditing(true)
            }}
          >
            {t('profile.edit')}
          </button>
        )}
      </div>

      <div className="p-5 space-y-3">
        {isEditing ? (
          <form onSubmit={handleSave} className="space-y-3">
            <div className="flex items-center gap-3">
              <Avatar
                name={displayNameInput || user?.email || ''}
                colorSeed={user?.id}
                avatarUrl={avatarUrlInput}
                size="lg"
              />
              <div className="flex-1">
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
                />
              </div>
            </div>
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
              />
            </div>

            <div className="flex gap-2">
              <Button type="submit" variant="primary" size="md" loading={isSaving}>
                {t('profile.save')}
              </Button>
              <Button
                type="button"
                variant="secondary"
                size="md"
                onClick={() => setIsEditing(false)}
                disabled={isSaving}
              >
                {t('profile.cancel')}
              </Button>
            </div>
          </form>
        ) : (
          <div className="flex items-center gap-3">
            <Avatar
              name={displayName || user?.email || ''}
              colorSeed={user?.id}
              avatarUrl={user?.avatarUrl}
              size="lg"
            />
            <div>
              <label className="text-xs font-medium text-text-secondary block mb-1">{t('profile.displayName')}</label>
              <p className="text-sm text-text-primary">
                {displayName || <span className="text-text-tertiary">{t('profile.notSet')}</span>}
              </p>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
