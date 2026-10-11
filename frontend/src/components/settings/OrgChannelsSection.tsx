import {useCallback, useId, useState} from 'react'
import {useTranslation} from 'react-i18next'
import type {TFunction} from 'i18next'
import {AlertTriangle, Bell, BellOff, KeyRound, Plus, Send, Trash2, Pencil} from 'lucide-react'
import {
  CHANNEL_EVENTS,
  CHANNEL_KINDS,
  LEGACY_CHANNEL_KINDS,
  channelCreateBody,
  channelEditBody,
  channelHasUrl,
  channelSigns,
  channelToggleBody,
  channelsApi,
  type ChannelEvent,
  type ChannelKind,
  type ChannelOptions,
  type KnownOrgChannel,
  type OrgChannel,
} from '@/api/governance'
import {ApiError} from '@/api/client'
import {Button, useToast} from '@/components/shared'
import {getErrorMessage} from '@/lib/getErrorMessage'
import {apiErrorField} from '@/lib/apiErrorField'
import {logger} from '@/lib/logger'
import {relativeTimeText} from '@/lib/time'
import {useAsync} from '@/hooks/useAsync'
import {useFormValidation, type FieldErrors} from '@/hooks/useFormValidation'
import {useConfirm} from '@/components/shared'
import type {OrgMember} from '@/stores/orgStore'
import {isKnown} from '@/types'
import clsx from 'clsx'

const isKnownChannel = (ch: OrgChannel): ch is KnownOrgChannel => isKnown(CHANNEL_KINDS, ch.kind)

const KINDS: {id: ChannelKind; labelKey: string; hintKey: string}[] = [
  {id: 'webhook', labelKey: 'channels.kindWebhook', hintKey: 'channels.kindWebhookHint'},
  {id: 'teams-workflow', labelKey: 'channels.kindTeamsWorkflow', hintKey: 'channels.kindTeamsWorkflowHint'},
  {id: 'teams', labelKey: 'channels.kindTeams', hintKey: 'channels.kindTeamsHint'},
  {id: 'slack', labelKey: 'channels.kindSlack', hintKey: 'channels.kindSlackHint'},
  {id: 'jira', labelKey: 'channels.kindJira', hintKey: 'channels.kindJiraHint'},
  {id: 'email', labelKey: 'channels.kindEmail', hintKey: 'channels.kindEmailHint'},
]

const EVENT_LABELS: Record<ChannelEvent, {labelKey: string; shortKey: string}> = {
  drift: {labelKey: 'channels.eventDrift', shortKey: 'channels.eventDriftShort'},
  health_regression: {labelKey: 'channels.eventHealthRegression', shortKey: 'channels.eventHealthRegressionShort'},
  new_rules: {labelKey: 'channels.eventNewRules', shortKey: 'channels.eventNewRulesShort'},
}

const channelEvents = (ch?: OrgChannel): ChannelEvent[] =>
  ch?.events ? CHANNEL_EVENTS.filter(e => ch.events?.includes(e)) : [...CHANNEL_EVENTS]

const memberName = (members: OrgMember[], id: string, t: TFunction<'settings'>): string => {
  const m = members.find(x => x.userId === id)
  return m ? m.user?.displayName || m.user?.email || m.userId : t('channels.formerMember')
}

function testFailure(e: unknown, t: TFunction<'settings'>): string {
  if (e instanceof ApiError) {
    if (e.code === 'CHANNEL_DELIVERIES_BUSY') return t('channels.testBusy')
    if (e.status === 429) return t('channels.testRateLimited')
    const {status, reason} = e.details ?? {}
    if (typeof status === 'number') return t('channels.testFailedStatus', {status})
    if (typeof reason === 'string' && reason) return reason.charAt(0).toUpperCase() + reason.slice(1)
  }
  return getErrorMessage(e)
}

export default function OrgChannelsSection({
  orgId,
  isAdmin,
  members = [],
}: {
  orgId: string
  isAdmin: boolean
  members?: OrgMember[]
}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const [editing, setEditing] = useState<KnownOrgChannel | null>(null)
  const [creating, setCreating] = useState(false)

  const {data, isLoading, error, refetch} = useAsync<OrgChannel[]>(signal => channelsApi.list(orgId, signal), [orgId])
  const channels = data ?? []
  const {data: options} = useAsync<ChannelOptions | null>(
    async signal => (isAdmin ? ((await channelsApi.options(orgId, signal)) ?? null) : null),
    [orgId, isAdmin],
  )

  const handleDelete = useCallback(
    async (ch: OrgChannel) => {
      try {
        await channelsApi.remove(orgId, ch.id)
        toast.success(t('channels.removed', {name: ch.name || ch.kind}))
        refetch()
      } catch (e) {
        toast.error(t('channels.removeFailed'), {description: getErrorMessage(e)})
      }
    },
    [orgId, refetch, t, toast],
  )

  const handleTest = useCallback(
    async (ch: OrgChannel) => {
      try {
        await channelsApi.test(orgId, ch.id)
        toast.success(t('channels.testSent'), {description: ch.name || ch.kind})
      } catch (e) {
        toast.error(t('channels.testFailed'), {description: testFailure(e, t)})
      }
    },
    [orgId, t, toast],
  )

  const handleToggle = useCallback(
    async (ch: KnownOrgChannel) => {
      try {
        await channelsApi.save(orgId, channelToggleBody(ch, !ch.enabled))
        refetch()
      } catch (e) {
        toast.error(t('channels.toggleFailed'), {description: getErrorMessage(e)})
      }
    },
    [orgId, refetch, t, toast],
  )

  const done = useCallback(() => {
    setEditing(null)
    setCreating(false)
    refetch()
  }, [refetch])

  return (
    <section className="mt-8">
      <div className="flex items-center justify-between mb-2">
        <div>
          <h3 className="text-sm font-medium text-text-primary">{t('channels.title')}</h3>
          <p className="text-xs text-text-tertiary mt-0.5">{t('channels.subtitle')}</p>
        </div>
        {isAdmin && !creating && !editing && (
          <Button variant="ghost" size="sm" icon={Plus} onClick={() => setCreating(true)}>
            {t('channels.add')}
          </Button>
        )}
      </div>

      {(creating || editing) && (
        <ChannelForm
          key={editing?.id ?? 'new'}
          orgId={orgId}
          initial={editing ?? undefined}
          options={options ?? null}
          members={members}
          onDone={done}
          onCancel={() => {
            setCreating(false)
            setEditing(null)
          }}
        />
      )}

      {isLoading && !data ? (
        <p className="text-xs text-text-tertiary py-3">{t('channels.loading')}</p>
      ) : error ? (
        <p className="text-xs text-red-400 py-3">{t('channels.loadFailed')}</p>
      ) : channels.length === 0 && !creating ? (
        <p className="text-xs text-text-tertiary py-3">{t('channels.empty')}</p>
      ) : (
        <ul className="space-y-1.5">
          {channels.map(ch => (
            <ChannelRow
              key={ch.id}
              ch={ch}
              isAdmin={isAdmin}
              members={members}
              onTest={handleTest}
              onToggle={handleToggle}
              onEdit={setEditing}
              onDelete={handleDelete}
            />
          ))}
        </ul>
      )}
    </section>
  )
}

function ChannelRow({
  ch,
  isAdmin,
  members,
  onTest,
  onToggle,
  onEdit,
  onDelete,
}: {
  ch: OrgChannel
  isAdmin: boolean
  members: OrgMember[]
  onTest: (ch: OrgChannel) => void
  onToggle: (ch: KnownOrgChannel) => void
  onEdit: (ch: KnownOrgChannel) => void
  onDelete: (ch: OrgChannel) => void
}) {
  const {t} = useTranslation('settings')
  const {confirm} = useConfirm()
  const kind = KINDS.find(k => k.id === ch.kind)
  const known = isKnownChannel(ch)
  const signed = ch.hasSecret === true && (!isKnownChannel(ch) || channelSigns(ch.kind))
  const canToggle = isAdmin && known && !ch.unreadable
  const events = channelEvents(ch)
  const where =
    ch.kind === 'email'
      ? t('channels.recipientsListed', {names: (ch.recipients ?? []).map(id => memberName(members, id, t)).join(', ')})
      : ch.url
  return (
    <li className="flex items-center gap-2 p-2 bg-surface-2 border border-border-default rounded-lg group">
      <button
        onClick={() => canToggle && isKnownChannel(ch) && onToggle(ch)}
        disabled={!canToggle}
        title={
          !known
            ? t('channels.unknownKind')
            : ch.unreadable
              ? t('channels.unreadableTitle')
              : ch.enabled
                ? t('channels.enabledTitle')
                : t('channels.pausedTitle')
        }
        aria-label={ch.enabled ? t('channels.pauseAria') : t('channels.enableAria')}
        className={clsx(
          'p-1.5 rounded shrink-0 transition-colors',
          ch.enabled ? 'text-green-500' : 'text-text-tertiary',
          isAdmin && 'hover:bg-surface-3',
        )}
      >
        {ch.enabled ? <Bell size={13} /> : <BellOff size={13} />}
      </button>
      <div className="flex-1 min-w-0">
        <div className="text-xs text-text-primary truncate">
          {ch.name || <span className="text-text-tertiary">{t('channels.unnamed')}</span>}
          <span className="text-text-tertiary/60"> · {kind ? t(kind.labelKey as never) : ch.kind}</span>
        </div>
        <div className="flex items-center gap-1 min-w-0 text-2xs text-text-tertiary">
          {ch.unreadable ? (
            <span
              className="inline-flex shrink-0 items-center gap-0.5 text-semantic-warning"
              title={t('channels.unreadableTitle')}
            >
              <AlertTriangle size={9} aria-hidden="true" />
              {t('channels.unreadable')}
            </span>
          ) : (
            <span className="truncate" title={where}>
              {where}
            </span>
          )}
          {ch.kind === 'jira' && ch.project && (
            <span className="shrink-0">· {t('channels.jiraProjectListed', {project: ch.project})}</span>
          )}
          {signed && (
            <>
              <span className="shrink-0">·</span>
              <span className="inline-flex shrink-0 items-center gap-0.5" title={t('channels.signedTitle')}>
                <KeyRound size={9} aria-hidden="true" />
                {t('channels.signed')}
              </span>
            </>
          )}
          {events.length < CHANNEL_EVENTS.length && (
            <span className="shrink-0">
              ·{' '}
              {t('channels.eventsListed', {
                events: events.map(e => t(EVENT_LABELS[e].shortKey as never)).join(', '),
              })}
            </span>
          )}
          <span className="shrink-0">· {t('channels.addedWhen', {when: relativeTimeText(ch.createdAt)})}</span>
        </div>
      </div>
      {isAdmin && (
        <div className="flex items-center gap-0.5 opacity-0 group-hover:opacity-100 transition-opacity">
          <button
            onClick={() => onTest(ch)}
            title={t('channels.testTitle')}
            aria-label={t('channels.testAria')}
            className="p-1.5 rounded text-text-tertiary hover:text-brand-400 hover:bg-surface-3"
          >
            <Send size={12} />
          </button>
          <button
            onClick={() => isKnownChannel(ch) && onEdit(ch)}
            disabled={!known}
            title={known ? t('channels.editTitle') : t('channels.unknownKind')}
            aria-label={t('channels.editAria')}
            className="p-1.5 rounded text-text-tertiary hover:text-text-secondary hover:bg-surface-3 disabled:opacity-40"
          >
            <Pencil size={12} />
          </button>
          <button
            onClick={() =>
              void confirm({
                title: t('channels.removeConfirmTitle'),
                message: t('channels.removeConfirmMessage', {name: ch.name || ch.kind}),
                danger: true,
                confirmLabel: t('channels.removeConfirmLabel'),
              }).then(ok => ok && onDelete(ch))
            }
            title={t('channels.removeTitle')}
            aria-label={t('channels.removeAria')}
            className="p-1.5 rounded text-text-tertiary hover:text-red-400 hover:bg-surface-3"
          >
            <Trash2 size={12} />
          </button>
        </div>
      )}
    </li>
  )
}

type FormField = 'kind' | 'url' | 'secret' | 'email' | 'token' | 'project' | 'recipients' | 'events'

const FORM_FIELDS: readonly FormField[] = ['kind', 'url', 'secret', 'email', 'token', 'project', 'recipients', 'events']

function fieldErrorOf(e: unknown): {field: FormField; message: string} | null {
  if (!(e instanceof ApiError) || e.status !== 400) return null
  const named = apiErrorField(e)
  return named && isKnown(FORM_FIELDS, named.field) ? {field: named.field, message: e.message} : null
}

const inputClass =
  'w-full px-2.5 py-1.5 bg-surface-3 border rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-hidden focus:border-brand-500 font-mono'

function ChannelForm({
  orgId,
  initial,
  options,
  members,
  onDone,
  onCancel,
}: {
  orgId: string
  initial?: KnownOrgChannel
  options: ChannelOptions | null
  members: OrgMember[]
  onDone: () => void
  onCancel: () => void
}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const uid = useId()
  const [name, setName] = useState(initial?.name ?? '')
  const [kind, setKind] = useState<ChannelKind>(initial?.kind ?? 'webhook')
  const [url, setUrl] = useState('')
  const [secret, setSecret] = useState('')
  const [clearSecret, setClearSecret] = useState(false)
  const [jiraEmail, setJiraEmail] = useState(initial?.email ?? '')
  const [token, setToken] = useState('')
  const [project, setProject] = useState(initial?.project ?? '')
  const [recipients, setRecipients] = useState<string[]>(initial?.recipients ?? [])
  const initialEvents = initial ? channelEvents(initial) : [...CHANNEL_EVENTS]
  const [events, setEvents] = useState<ChannelEvent[]>(initialEvents)
  const [fieldError, setFieldError] = useState<{field: FormField; message: string} | null>(null)
  const [saving, setSaving] = useState(false)

  const offered = KINDS.filter(k =>
    options ? options.kinds.includes(k.id) || k.id === initial?.kind : LEGACY_CHANNEL_KINDS.includes(k.id),
  )
  const signs = channelSigns(kind)
  const hasUrl = channelHasUrl(kind)
  const v = useFormValidation<FormField>({
    fields: FORM_FIELDS,
    validate: () => {
      const errors: FieldErrors<FormField> = {}
      if (hasUrl && (!initial || initial.unreadable) && !url.trim())
        errors.url = initial?.unreadable ? t('channels.unreadableHint') : t('channels.urlRequired')
      if (kind === 'jira') {
        if (!jiraEmail.trim()) errors.email = t('errors.required', {ns: 'common'})
        if (!initial?.hasToken && !token.trim()) errors.token = t('errors.required', {ns: 'common'})
        if (!project.trim()) errors.project = t('errors.required', {ns: 'common'})
      }
      if (kind === 'email' && recipients.length === 0) errors.recipients = t('channels.recipientsRequired')
      if (events.length === 0) errors.events = t('channels.eventsRequired')
      return errors
    },
  })
  const errorOf = (field: FormField) =>
    fieldError?.field === field ? fieldError.message : (v.errorFor(field) ?? null)
  const errorId = (field: FormField) => `${uid}-${field}-error`
  const clearError = (field: FormField) => {
    if (fieldError?.field === field) setFieldError(null)
  }
  const changedEvents =
    events.length !== initialEvents.length || events.some(e => !initialEvents.includes(e)) ? events : undefined

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (v.submitValidate()) return
    const fields = {
      email: jiraEmail,
      token,
      project,
      recipients,
      events: changedEvents,
    }
    setSaving(true)
    setFieldError(null)
    try {
      await channelsApi.save(
        orgId,
        initial
          ? channelEditBody(initial, {name, url, secret, clearSecret, ...fields})
          : channelCreateBody({name, kind, url, secret, ...fields}),
      )
      toast.success(initial ? t('channels.updated') : t('channels.added'))
      onDone()
    } catch (err) {
      logger.warn(err)
      const fe = fieldErrorOf(err)
      if (fe && (fe.field !== 'secret' || (signs && !clearSecret))) setFieldError(fe)
      else toast.error(t('channels.saveFailed'), {description: getErrorMessage(err)})
    } finally {
      setSaving(false)
    }
  }

  const activeKind = KINDS.find(k => k.id === kind)
  const urlDescribedBy = [
    initial && !initial.unreadable && `${uid}-url-hint`,
    options?.hostsRestricted && `${uid}-hosts-hint`,
    errorOf('url') && errorId('url'),
  ]
    .filter(Boolean)
    .join(' ')

  const errorText = (field: FormField) =>
    errorOf(field) ? (
      <p id={errorId(field)} className="text-2xs text-semantic-error">
        {errorOf(field)}
      </p>
    ) : null
  const fieldProps = (field: FormField) => ({
    'aria-invalid': !!errorOf(field) || undefined,
    'aria-describedby': errorOf(field) ? errorId(field) : undefined,
    className: clsx(inputClass, errorOf(field) ? 'border-semantic-error' : 'border-border-default'),
    onBlur: () => v.touch(field),
  })
  const choosable = [...members.map(m => m.userId), ...recipients.filter(id => !members.some(m => m.userId === id))]

  return (
    <form
      onSubmit={handleSubmit}
      className="p-3 bg-surface-2 border border-border-default rounded-lg mb-2 space-y-2.5"
      data-testid="channel-form"
    >
      <div className="flex gap-2">
        <input
          value={name}
          onChange={e => setName(e.target.value)}
          placeholder={t('channels.namePlaceholder')}
          className="flex-1 px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-hidden focus:border-brand-500"
        />
        <select
          value={kind}
          onChange={e => {
            setKind(e.target.value as ChannelKind)
            setFieldError(null)
          }}
          disabled={!!initial}
          className={clsx(
            'px-2 py-1.5 bg-surface-3 border rounded-md text-xs text-text-primary outline-hidden focus:border-brand-500 disabled:opacity-60',
            errorOf('kind') ? 'border-semantic-error' : 'border-border-default',
          )}
          aria-label={t('channels.kindAria')}
          aria-invalid={!!errorOf('kind') || undefined}
          aria-describedby={errorOf('kind') ? errorId('kind') : undefined}
        >
          {offered.map(k => (
            <option key={k.id} value={k.id}>
              {t(k.labelKey as never)}
            </option>
          ))}
        </select>
      </div>
      {errorText('kind')}
      {hasUrl && (
        <>
          <input
            value={url}
            onChange={e => {
              setUrl(e.target.value)
              clearError('url')
            }}
            placeholder={
              initial && !initial.unreadable
                ? initial.url
                : kind === 'jira'
                  ? t('channels.jiraUrlPlaceholder')
                  : 'https://hooks.example.com/…'
            }
            aria-label={t('channels.urlAria')}
            aria-invalid={!!errorOf('url') || undefined}
            aria-describedby={urlDescribedBy || undefined}
            className={clsx(inputClass, errorOf('url') ? 'border-semantic-error' : 'border-border-default')}
            onBlur={() => v.touch('url')}
          />
          {initial && !initial.unreadable && (
            <p id={`${uid}-url-hint`} className="text-2xs text-text-tertiary/70">
              {t('channels.urlKeepHint')}
            </p>
          )}
          {options?.hostsRestricted && (
            <p id={`${uid}-hosts-hint`} className="text-2xs text-text-tertiary/70">
              {options.allowedHosts?.length
                ? t('channels.hostsAllowed', {hosts: options.allowedHosts.join(', ')})
                : t('channels.hostsRestricted')}
            </p>
          )}
          {errorText('url')}
        </>
      )}
      {kind === 'jira' && (
        <>
          <input
            value={jiraEmail}
            onChange={e => {
              setJiraEmail(e.target.value)
              clearError('email')
            }}
            placeholder={t('channels.jiraEmailPlaceholder')}
            aria-label={t('channels.jiraEmailAria')}
            autoComplete="off"
            {...fieldProps('email')}
          />
          {errorText('email')}
          <input
            value={token}
            onChange={e => {
              setToken(e.target.value)
              clearError('token')
            }}
            placeholder={
              initial?.hasToken ? t('channels.jiraTokenPlaceholderReplace') : t('channels.jiraTokenPlaceholderNew')
            }
            type="password"
            autoComplete="new-password"
            aria-label={t('channels.jiraTokenAria')}
            {...fieldProps('token')}
          />
          {errorText('token')}
          <input
            value={project}
            onChange={e => {
              setProject(e.target.value)
              clearError('project')
            }}
            placeholder={t('channels.jiraProjectPlaceholder')}
            aria-label={t('channels.jiraProjectAria')}
            {...fieldProps('project')}
          />
          {errorText('project')}
        </>
      )}
      {kind === 'email' && (
        <fieldset
          className="space-y-1"
          aria-invalid={!!errorOf('recipients') || undefined}
          aria-describedby={[`${uid}-recipients-hint`, errorOf('recipients') && errorId('recipients')]
            .filter(Boolean)
            .join(' ')}
        >
          <legend className="text-2xs text-text-secondary">{t('channels.recipientsLabel')}</legend>
          {choosable.length === 0 ? (
            <p className="text-2xs text-text-tertiary">{t('channels.recipientsNone')}</p>
          ) : (
            choosable.map(id => (
              <label key={id} className="flex items-center gap-2 text-xs text-text-primary">
                <input
                  type="checkbox"
                  checked={recipients.includes(id)}
                  onChange={e => {
                    setRecipients(prev => (e.target.checked ? [...prev, id] : prev.filter(r => r !== id)))
                    clearError('recipients')
                  }}
                />
                {memberName(members, id, t)}
              </label>
            ))
          )}
          <p id={`${uid}-recipients-hint`} className="text-2xs text-text-tertiary/70">
            {t('channels.recipientsHint')}
          </p>
          {errorText('recipients')}
        </fieldset>
      )}
      {signs &&
        (clearSecret ? (
          <div className="flex items-center justify-between gap-2">
            <p className="text-2xs text-text-secondary">{t('channels.secretWillBeRemoved')}</p>
            <Button variant="ghost" size="sm" type="button" onClick={() => setClearSecret(false)}>
              {t('channels.keepSecret')}
            </Button>
          </div>
        ) : (
          <div className="flex items-center gap-2">
            <input
              value={secret}
              onChange={e => {
                setSecret(e.target.value)
                clearError('secret')
              }}
              placeholder={
                !initial || initial.hasSecret === false
                  ? t('channels.secretPlaceholderNew')
                  : initial.hasSecret
                    ? t('channels.secretPlaceholderReplace')
                    : t('channels.secretPlaceholderExisting')
              }
              type="password"
              autoComplete="new-password"
              aria-label={t('channels.secretAria')}
              aria-invalid={!!errorOf('secret') || undefined}
              aria-describedby={errorOf('secret') ? errorId('secret') : undefined}
              className={clsx(
                inputClass,
                'flex-1',
                errorOf('secret') ? 'border-semantic-error' : 'border-border-default',
              )}
            />
            {initial?.hasSecret && (
              <Button
                variant="ghost"
                size="sm"
                type="button"
                onClick={() => {
                  setClearSecret(true)
                  setSecret('')
                  clearError('secret')
                }}
              >
                {t('channels.removeSecret')}
              </Button>
            )}
          </div>
        ))}
      {signs && !clearSecret && errorText('secret')}
      <fieldset className="space-y-1" aria-describedby={`${uid}-events-hint`}>
        <legend className="text-2xs text-text-secondary">{t('channels.eventsLabel')}</legend>
        {CHANNEL_EVENTS.map(ev => (
          <label key={ev} className="flex items-center gap-2 text-xs text-text-primary">
            <input
              type="checkbox"
              checked={events.includes(ev)}
              onChange={e => {
                setEvents(prev =>
                  e.target.checked
                    ? CHANNEL_EVENTS.filter(x => x === ev || prev.includes(x))
                    : prev.filter(x => x !== ev),
                )
                clearError('events')
              }}
            />
            {t(EVENT_LABELS[ev].labelKey as never)}
          </label>
        ))}
        <p id={`${uid}-events-hint`} className="text-2xs text-text-tertiary/70">
          {t('channels.eventsHint')}
        </p>
        {errorText('events')}
      </fieldset>
      {activeKind && (
        <p className="text-2xs text-text-tertiary/70">
          {hasUrl
            ? t('channels.formHint', {kind: t(activeKind.hintKey as never)})
            : t('channels.formHintNoUrl', {kind: t(activeKind.hintKey as never)})}
        </p>
      )}
      <div className="flex justify-end gap-2">
        <Button variant="ghost" size="sm" onClick={onCancel} type="button">
          {t('channels.cancel')}
        </Button>
        <Button variant="primary" size="sm" disabled={saving} type="submit">
          {saving ? t('channels.saving') : initial ? t('channels.save') : t('channels.addAction')}
        </Button>
      </div>
    </form>
  )
}
