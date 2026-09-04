import {useCallback, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Bell, BellOff, Plus, Send, Trash2, Pencil} from 'lucide-react'
import {channelsApi, type ChannelKind, type OrgChannel} from '@/api/governance'
import {Button, useToast} from '@/components/shared'
import {logger} from '@/lib/logger'
import {relativeTime} from '@/lib/time'
import {useAsync} from '@/hooks/useAsync'
import {useConfirm} from '@/components/shared'
import clsx from 'clsx'

// OrgChannelsSection manages an org's own notification destinations
// (webhook/Teams/Slack): governance events for the org's flows are delivered
// here IN ADDITION to the deployment-global channels — routing by
// ownership. Admin-only edits; members read.
// Keys, not literals: this list is module-level and cannot call t(). Each entry
// names its label/hint keys, resolved at render time by the consuming component.
const KINDS: {id: ChannelKind; labelKey: string; hintKey: string}[] = [
  {id: 'webhook', labelKey: 'channels.kindWebhook', hintKey: 'channels.kindWebhookHint'},
  {id: 'teams', labelKey: 'channels.kindTeams', hintKey: 'channels.kindTeamsHint'},
  {id: 'slack', labelKey: 'channels.kindSlack', hintKey: 'channels.kindSlackHint'},
]

export default function OrgChannelsSection({orgId, isAdmin}: {orgId: string; isAdmin: boolean}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const [editing, setEditing] = useState<OrgChannel | null>(null)
  const [creating, setCreating] = useState(false)

  const {data, isLoading, error, refetch} = useAsync<OrgChannel[]>(() => channelsApi.list(orgId), [orgId])
  const channels = data ?? []

  const handleDelete = useCallback(
    async (ch: OrgChannel) => {
      try {
        await channelsApi.remove(orgId, ch.id)
        toast.success(t('channels.removed', {name: ch.name || ch.kind}))
        refetch()
      } catch (e) {
        toast.error(t('channels.removeFailed'), {description: String(e)})
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
        toast.error(t('channels.testFailed'), {description: String(e)})
      }
    },
    [orgId, t, toast],
  )

  const handleToggle = useCallback(
    async (ch: OrgChannel) => {
      try {
        await channelsApi.save(orgId, {
          id: ch.id,
          name: ch.name,
          kind: ch.kind,
          url: ch.url,
          enabled: !ch.enabled,
        })
        refetch()
      } catch (e) {
        toast.error(t('channels.toggleFailed'), {description: String(e)})
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
          orgId={orgId}
          initial={editing ?? undefined}
          onDone={done}
          onCancel={() => {
            setCreating(false)
            setEditing(null)
          }}
        />
      )}

      {isLoading ? (
        <p className="text-xs text-text-tertiary py-3">{t('channels.loading')}</p>
      ) : error ? (
        <p className="text-xs text-red-400 py-3">{t('channels.loadFailed')}</p>
      ) : channels.length === 0 && !creating ? (
        <p className="text-xs text-text-tertiary py-3">
          No channels configured. Events go only to the deployment-global destinations.
        </p>
      ) : (
        <ul className="space-y-1.5">
          {channels.map(ch => (
            <ChannelRow
              key={ch.id}
              ch={ch}
              isAdmin={isAdmin}
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
  onTest,
  onToggle,
  onEdit,
  onDelete,
}: {
  ch: OrgChannel
  isAdmin: boolean
  onTest: (ch: OrgChannel) => void
  onToggle: (ch: OrgChannel) => void
  onEdit: (ch: OrgChannel) => void
  onDelete: (ch: OrgChannel) => void
}) {
  const {t} = useTranslation('settings')
  const {confirm} = useConfirm()
  const kind = KINDS.find(k => k.id === ch.kind)
  return (
    <li className="flex items-center gap-2 p-2 bg-surface-2 border border-border-default rounded-lg group">
      <button
        onClick={() => isAdmin && onToggle(ch)}
        disabled={!isAdmin}
        title={ch.enabled ? t('channels.enabledTitle') : t('channels.pausedTitle')}
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
        <div className="text-2xs text-text-tertiary truncate" title={ch.url}>
          {ch.url} · added {relativeTime(ch.createdAt)}
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
            onClick={() => onEdit(ch)}
            title={t('channels.editTitle')}
            aria-label={t('channels.editAria')}
            className="p-1.5 rounded text-text-tertiary hover:text-text-secondary hover:bg-surface-3"
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

function ChannelForm({
  orgId,
  initial,
  onDone,
  onCancel,
}: {
  orgId: string
  initial?: OrgChannel
  onDone: () => void
  onCancel: () => void
}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const [name, setName] = useState(initial?.name ?? '')
  const [kind, setKind] = useState<ChannelKind>(initial?.kind ?? 'webhook')
  const [url, setUrl] = useState(initial?.url ?? '')
  const [secret, setSecret] = useState('')
  const [saving, setSaving] = useState(false)

  // The secret is never returned by the API; editing keeps the stored one
  // unless a new value is typed.
  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!url.trim()) {
      toast.error(t('channels.urlRequired'))
      return
    }
    setSaving(true)
    try {
      await channelsApi.save(orgId, {
        id: initial?.id,
        name: name.trim(),
        kind,
        url: url.trim(),
        secret: secret.trim() || undefined,
      })
      toast.success(initial ? t('channels.updated') : t('channels.added'))
      onDone()
    } catch (err) {
      logger.warn(err)
      toast.error(t('channels.saveFailed'), {description: String(err)})
    } finally {
      setSaving(false)
    }
  }

  const activeKind = KINDS.find(k => k.id === kind)

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
          className="flex-1 px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-none focus:border-brand-500"
        />
        <select
          value={kind}
          onChange={e => setKind(e.target.value as ChannelKind)}
          disabled={!!initial}
          className="px-2 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary outline-none focus:border-brand-500 disabled:opacity-60"
          aria-label={t('channels.kindAria')}
        >
          {KINDS.map(k => (
            <option key={k.id} value={k.id}>
              {t(k.labelKey as never)}
            </option>
          ))}
        </select>
      </div>
      <input
        value={url}
        onChange={e => setUrl(e.target.value)}
        placeholder="https://hooks.example.com/…"
        className="w-full px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-none focus:border-brand-500 font-mono"
      />
      <input
        value={secret}
        onChange={e => setSecret(e.target.value)}
        placeholder={initial ? t('channels.secretPlaceholderExisting') : t('channels.secretPlaceholderNew')}
        type="password"
        className="w-full px-2.5 py-1.5 bg-surface-3 border border-border-default rounded-md text-xs text-text-primary placeholder:text-text-tertiary/60 outline-none focus:border-brand-500 font-mono"
      />
      {activeKind && (
        <p className="text-2xs text-text-tertiary/70">{t(activeKind.hintKey as never)}. HTTPS required.</p>
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
