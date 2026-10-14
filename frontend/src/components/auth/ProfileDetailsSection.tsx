import {getErrorMessage} from '@/lib/getErrorMessage'
import {useTranslation} from 'react-i18next'
import React, {useMemo, useState} from 'react'
import {Briefcase, Clock, Pencil} from 'lucide-react'
import {useSettingsStore} from '@/stores/settingsStore'
import {Button, Input, Select, Textarea, useToast} from '@/components/shared'
import {useFormValidation} from '@/hooks/useFormValidation'
import {PROFILE_BIO_MAX_LENGTH, PROFILE_JOB_TITLE_MAX_LENGTH} from '@/lib/constraints'
import {SectionCard} from './SectionCard'

// The identity-card fields (the settings document's profile section): a short bio, what the user does, and the
// timezone their own timestamps render in. Saved through the settings store, so the same debounced persist path
// every other settings panel uses carries it; the caps mirror validateProfileSection (internal/service/system.go).
export const ProfileDetailsSection: React.FC = () => {
  const {t} = useTranslation('auth')
  const profile = useSettingsStore(s => s.settings.profile)
  const isLoaded = useSettingsStore(s => s.isLoaded)
  const updateProfile = useSettingsStore(s => s.updateProfile)
  const toast = useToast()

  const [editing, setEditing] = useState(false)
  const [bio, setBio] = useState(profile.bio ?? '')
  const [jobTitle, setJobTitle] = useState(profile.jobTitle ?? '')
  const [timezone, setTimezone] = useState(profile.timezone ?? '')
  const [isSaving, setIsSaving] = useState(false)

  // A profile saved elsewhere (or loaded after mount) re-seeds the view fields; while editing, the form keeps
  // what the user is typing. Render-phase sync, the pattern ProfileSection uses for its edit toggle.
  const [synced, setSynced] = useState(profile)
  if (!editing && profile !== synced) {
    setSynced(profile)
    setBio(profile.bio ?? '')
    setJobTitle(profile.jobTitle ?? '')
    setTimezone(profile.timezone ?? '')
  }

  const timezones = useMemo(() => {
    try {
      return Intl.supportedValuesOf('timeZone')
    } catch {
      return []
    }
  }, [])
  const timezoneOptions = useMemo(
    () => [
      {value: '', label: t('details.timezoneDevice')},
      ...timezones.map(tz => ({value: tz, label: tz.replace(/_/g, ' ')})),
    ],
    [timezones, t],
  )

  const fields = ['bio', 'jobTitle'] as const
  const {fieldProps, submitValidate, reset} = useFormValidation<(typeof fields)[number]>({
    fields,
    validate: () => ({
      bio: bio.length > PROFILE_BIO_MAX_LENGTH ? t('details.bioTooLong', {max: PROFILE_BIO_MAX_LENGTH}) : undefined,
      jobTitle:
        jobTitle.length > PROFILE_JOB_TITLE_MAX_LENGTH
          ? t('details.jobTitleTooLong', {max: PROFILE_JOB_TITLE_MAX_LENGTH})
          : undefined,
    }),
  })

  const dirty = bio !== (profile.bio ?? '') || jobTitle !== (profile.jobTitle ?? '') || timezone !== (profile.timezone ?? '')

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    if (submitValidate() !== null) return
    setIsSaving(true)
    try {
      await updateProfile({bio: bio.trim(), jobTitle: jobTitle.trim(), timezone})
      toast.success(t('details.updated'))
      setEditing(false)
      reset()
    } catch (err) {
      toast.error(t('details.updateFailed'), {description: getErrorMessage(err)})
    } finally {
      setIsSaving(false)
    }
  }

  const startEditing = () => {
    setBio(profile.bio ?? '')
    setJobTitle(profile.jobTitle ?? '')
    setTimezone(profile.timezone ?? '')
    reset()
    setEditing(true)
  }

  return (
    <SectionCard
      id="profile-details"
      icon={Briefcase}
      title={t('details.heading')}
      description={t('details.description')}
      action={
        !editing ? (
          <Button variant="ghost" size="sm" icon={Pencil} onClick={startEditing} disabled={!isLoaded}>
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
              setEditing(false)
            }
          }}
        >
          <div>
            <label htmlFor="profile-bio" className="text-xs font-medium text-text-secondary block mb-1.5">
              {t('details.bio')}
            </label>
            <Textarea
              id="profile-bio"
              value={bio}
              onChange={e => setBio(e.target.value)}
              placeholder={t('details.bioPlaceholder')}
              maxLength={PROFILE_BIO_MAX_LENGTH}
              rows={3}
              {...fieldProps('bio')}
            />
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <label htmlFor="profile-job-title" className="text-xs font-medium text-text-secondary block mb-1.5">
                {t('details.jobTitle')}
              </label>
              <Input
                id="profile-job-title"
                type="text"
                value={jobTitle}
                onChange={e => setJobTitle(e.target.value)}
                placeholder={t('details.jobTitlePlaceholder')}
                maxLength={PROFILE_JOB_TITLE_MAX_LENGTH}
                {...fieldProps('jobTitle')}
              />
            </div>
            <div>
              <label htmlFor="profile-timezone" className="text-xs font-medium text-text-secondary block mb-1.5">
                {t('details.timezone')}
              </label>
              <Select id="profile-timezone" value={timezone} onChange={e => setTimezone(e.target.value)}>
                {timezoneOptions.map(o => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </Select>
            </div>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" variant="primary" size="md" loading={isSaving} disabled={!dirty}>
              {t('profile.save')}
            </Button>
            <Button type="button" variant="secondary" size="md" onClick={() => setEditing(false)} disabled={isSaving}>
              {t('profile.cancel')}
            </Button>
          </div>
        </form>
      ) : (
        <dl className="space-y-3">
          <div className="profile-field">
            <dt className="text-xs font-medium text-text-secondary">{t('details.bio')}</dt>
            <dd className="text-sm text-text-primary min-w-0 whitespace-pre-wrap">
              {profile.bio?.trim() || <span className="text-text-tertiary">{t('profile.notSet')}</span>}
            </dd>
          </div>
          <div className="profile-field">
            <dt className="text-xs font-medium text-text-secondary">{t('details.jobTitle')}</dt>
            <dd className="text-sm text-text-primary min-w-0">
              {profile.jobTitle?.trim() || <span className="text-text-tertiary">{t('profile.notSet')}</span>}
            </dd>
          </div>
          <div className="profile-field">
            <dt className="text-xs font-medium text-text-secondary flex items-center gap-1">
              <Clock size={12} aria-hidden />
              {t('details.timezone')}
            </dt>
            <dd className="text-sm text-text-primary min-w-0">
              {profile.timezone || <span className="text-text-tertiary">{t('details.timezoneDevice')}</span>}
            </dd>
          </div>
        </dl>
      )}
    </SectionCard>
  )
}
