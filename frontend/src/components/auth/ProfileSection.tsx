import {getErrorMessage} from '@/lib/getErrorMessage'
import {useTranslation} from 'react-i18next'
import React, {useState, useCallback, useRef} from 'react'
import {Check, Copy, ImagePlus, Pencil, Trash2, User as UserIcon} from 'lucide-react'
import {useAuthStore} from '@/stores/authStore'
import {authApi} from '@/api/auth'
import {Avatar, Button, IconButton, Input, useToast} from '@/components/shared'
import {useCopy} from '@/hooks/useCopy'
import {httpUrlErrorKey} from '@/lib/validators'
import {
  AVATAR_ACCEPTED_TYPES,
  AVATAR_UPLOAD_MAX_BYTES,
  AVATAR_URL_MAX_LENGTH,
  DISPLAY_NAME_MAX_LENGTH,
} from '@/lib/constraints'
import {downscaleImage} from '@/lib/imageResize'
import {invalidateAvatar, isInternalAvatarUrl} from '@/lib/avatarCache'
import {SectionCard} from './SectionCard'

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
  const [isUploading, setIsUploading] = useState(false)
  const [avatarError, setAvatarError] = useState<string | undefined>()
  const fileInput = useRef<HTMLInputElement>(null)

  const [wasEditing, setWasEditing] = useState(editing)
  if (editing !== wasEditing) {
    setWasEditing(editing)
    if (editing) {
      setDisplayNameInput(user?.displayName ?? '')
      setAvatarUrlInput(user?.avatarUrl ?? '')
      setAvatarError(undefined)
    }
  }

  const nextName = displayNameInput.trim()
  const nextAvatar = avatarUrlInput.trim()
  // The upload's own serving route is what the field holds after an upload; it is not an http(s) URL, but saving
  // the name with it must keep the picture.
  const keepsUpload = nextAvatar !== '' && nextAvatar === user?.avatarUrl && isInternalAvatarUrl(nextAvatar)
  const avatarUrlKey = keepsUpload ? null : httpUrlErrorKey(nextAvatar)
  const avatarUrlError = avatarUrlKey ? t(avatarUrlKey, {ns: 'common', max: AVATAR_URL_MAX_LENGTH}) : undefined
  const dirty = nextName !== (user?.displayName ?? '') || nextAvatar !== (user?.avatarUrl ?? '')

  const handleSave = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      if (!dirty || avatarUrlError) return
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
    [dirty, avatarUrlError, nextName, nextAvatar, updateUser, toast, t, onEditingChange],
  )

  // The upload is immediate, not part of the form's save: the image is re-encoded and downscaled on the device,
  // the server strips its metadata, and the profile's avatar URL moves to the serving route.
  const handleFile = useCallback(
    async (file: File | undefined) => {
      if (!file) return
      setAvatarError(undefined)
      if (!AVATAR_ACCEPTED_TYPES.split(',').includes(file.type)) {
        setAvatarError(t('profile.avatarWrongType'))
        return
      }
      if (file.size > AVATAR_UPLOAD_MAX_BYTES) {
        setAvatarError(t('profile.avatarTooLarge', {max: Math.floor(AVATAR_UPLOAD_MAX_BYTES / (1024 * 1024))}))
        return
      }
      setIsUploading(true)
      try {
        const blob = await downscaleImage(file)
        const updated = await authApi.uploadAvatar(blob)
        const previous = user?.avatarUrl
        if (previous && isInternalAvatarUrl(previous)) invalidateAvatar(previous)
        updateUser({displayName: updated.displayName, avatarUrl: updated.avatarUrl})
        setAvatarUrlInput(updated.avatarUrl ?? '')
        toast.success(t('profile.avatarUpdated'))
      } catch (err) {
        setAvatarError(getErrorMessage(err))
      } finally {
        setIsUploading(false)
        if (fileInput.current) fileInput.current.value = ''
      }
    },
    [user, updateUser, toast, t],
  )

  const handleRemoveAvatar = useCallback(async () => {
    setAvatarError(undefined)
    setIsUploading(true)
    try {
      const previous = user?.avatarUrl
      if (previous && isInternalAvatarUrl(previous)) invalidateAvatar(previous)
      const updated = await authApi.deleteAvatar()
      updateUser({displayName: updated.displayName, avatarUrl: updated.avatarUrl})
      setAvatarUrlInput('')
      toast.success(t('profile.avatarRemoved'))
    } catch (err) {
      toast.error(t('profile.avatarRemoveFailed'), {description: getErrorMessage(err)})
    } finally {
      setIsUploading(false)
    }
  }, [user, updateUser, toast, t])

  if (!user) return null
  const displayName = user.displayName?.trim()
  const hasUpload = isInternalAvatarUrl(user.avatarUrl)
  const shownAvatarUrl = editing ? nextAvatar || user.avatarUrl : user.avatarUrl

  const avatarPicker = (
    <div className="flex flex-col gap-2">
      <Avatar
        name={nextName || user.email}
        colorSeed={user.id}
        avatarUrl={shownAvatarUrl}
        size="xl"
        className={isUploading ? 'opacity-50' : undefined}
      />
      <div className="flex flex-wrap items-center gap-2">
        <input
          ref={fileInput}
          type="file"
          accept={AVATAR_ACCEPTED_TYPES}
          className="sr-only"
          onChange={e => void handleFile(e.target.files?.[0])}
          disabled={isUploading}
        />
        <Button
          type="button"
          variant="secondary"
          size="sm"
          icon={ImagePlus}
          loading={isUploading}
          onClick={() => fileInput.current?.click()}
        >
          {t('profile.avatarUpload')}
        </Button>
        {hasUpload && (
          <IconButton
            icon={Trash2}
            size="sm"
            label={t('profile.avatarRemove')}
            onClick={() => void handleRemoveAvatar()}
            disabled={isUploading}
          />
        )}
      </div>
      {avatarError && (
        <p role="alert" className="text-xs text-danger">
          {avatarError}
        </p>
      )}
    </div>
  )

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
            {avatarPicker}
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
                  maxLength={DISPLAY_NAME_MAX_LENGTH}
                  autoFocus
                />
              </div>
              <div>
                <label htmlFor="profile-avatar-url" className="text-xs font-medium text-text-secondary block mb-1.5">
                  {t('profile.avatarUrl')}
                </label>
                <Input
                  id="profile-avatar-url"
                  // Not type="url": after an upload the field holds a relative route, which the browser's own URL
                  // check would refuse to submit; httpUrlErrorKey validates it.
                  type="text"
                  inputMode="url"
                  value={avatarUrlInput}
                  onChange={e => setAvatarUrlInput(e.target.value)}
                  placeholder={t('profile.avatarPlaceholder')}
                  maxLength={AVATAR_URL_MAX_LENGTH}
                  error={avatarUrlError}
                  hint={avatarUrlError ? undefined : t('profile.avatarUrlHint')}
                />
              </div>
            </div>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" variant="primary" size="md" loading={isSaving} disabled={!dirty || !!avatarUrlError}>
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
        <div className="flex items-start gap-4">
          <Avatar name={displayName || user.email} colorSeed={user.id} avatarUrl={user.avatarUrl} size="lg" />
          <dl className="space-y-3 flex-1 min-w-0">
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
            {!hasUpload && (
              <Field label={t('profile.avatarUrl')}>
                {user.avatarUrl ? (
                  <span className="block truncate" title={user.avatarUrl}>
                    {user.avatarUrl}
                  </span>
                ) : (
                  <span className="text-text-tertiary">{t('profile.notSet')}</span>
                )}
              </Field>
            )}
          </dl>
        </div>
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
