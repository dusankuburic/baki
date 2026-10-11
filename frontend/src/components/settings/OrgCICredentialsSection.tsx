import {useCallback, useId, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {AlertTriangle, Check, Copy, KeyRound, Plus, Trash2} from 'lucide-react'
import clsx from 'clsx'
import {ciCredentialsApi, type CreatedOrgCICredential, type OrgCICredential} from '@/api/governance'
import {ApiError, getBackendConfig} from '@/api/client'
import type {OrgMember} from '@/stores/orgStore'
import {Badge, Button, FormField, Input, Modal, useConfirm, useToast} from '@/components/shared'
import {useAsync} from '@/hooks/useAsync'
import {useFormValidation, type FieldErrors} from '@/hooks/useFormValidation'
import {CI_CREDENTIAL_MAX_EXPIRY_DAYS, CI_CREDENTIAL_MAX_NAME} from '@/lib/constraints'
import {writeClipboard} from '@/lib/clipboard'
import {getErrorMessage} from '@/lib/getErrorMessage'
import {logger} from '@/lib/logger'
import {absoluteTime, relativeTimeText} from '@/lib/time'

const isExpired = (iso?: string | null): boolean => !!iso && new Date(iso).getTime() <= Date.now()

async function apiOrigin(): Promise<string> {
  try {
    return (await getBackendConfig()).apiUrl.replace(/\/+$/, '')
  } catch {
    return window.location.origin
  }
}

export default function OrgCICredentialsSection({orgId, members}: {orgId: string; members: OrgMember[]}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const {confirm} = useConfirm()
  const [creating, setCreating] = useState(false)

  const {data, isLoading, error, refetch} = useAsync<OrgCICredential[]>(
    signal => ciCredentialsApi.list(orgId, signal),
    [orgId],
  )
  const credentials = data ?? []

  const creatorName = (cred: OrgCICredential) => {
    const member = members.find(m => m.userId === cred.createdBy)
    if (member) return member.user?.displayName || member.user?.email || member.userId
    return cred.createdByName || t('ciCredentials.formerMember')
  }

  const handleRevoke = useCallback(
    async (cred: OrgCICredential) => {
      const name = cred.name || cred.id
      const ok = await confirm({
        title: t('ciCredentials.revokeConfirmTitle'),
        message: t('ciCredentials.revokeConfirmMessage', {name}),
        danger: true,
        confirmLabel: t('ciCredentials.revokeConfirmLabel'),
      })
      if (!ok) return
      try {
        await ciCredentialsApi.revoke(orgId, cred.id)
        toast.success(t('ciCredentials.revoked', {name}))
      } catch (e) {
        toast.error(t('ciCredentials.revokeFailed'), {description: getErrorMessage(e)})
      }
      refetch()
    },
    [confirm, orgId, refetch, t, toast],
  )

  return (
    <section className="mt-8">
      <div className="flex items-center justify-between mb-2">
        <div>
          <h3 className="text-sm font-medium text-text-primary">{t('ciCredentials.title')}</h3>
          <p className="text-xs text-text-tertiary mt-0.5">{t('ciCredentials.subtitle')}</p>
        </div>
        <Button variant="ghost" size="sm" icon={Plus} onClick={() => setCreating(true)}>
          {t('ciCredentials.add')}
        </Button>
      </div>

      {creating && <CreateDialog orgId={orgId} onCreated={refetch} onClose={() => setCreating(false)} />}

      {isLoading && !data ? (
        <p className="text-xs text-text-tertiary py-3">{t('ciCredentials.loading')}</p>
      ) : error ? (
        <p className="text-xs text-red-400 py-3">{t('ciCredentials.loadFailed')}</p>
      ) : credentials.length === 0 ? (
        <p className="text-xs text-text-tertiary py-3">{t('ciCredentials.empty')}</p>
      ) : (
        <ul className="space-y-1.5">
          {credentials.map(cred => (
            <CredentialRow key={cred.id} cred={cred} creator={creatorName(cred)} onRevoke={handleRevoke} />
          ))}
        </ul>
      )}
    </section>
  )
}

function CredentialRow({
  cred,
  creator,
  onRevoke,
}: {
  cred: OrgCICredential
  creator: string
  onRevoke: (cred: OrgCICredential) => void
}) {
  const {t} = useTranslation('settings')
  const name = cred.name || cred.id
  const expired = isExpired(cred.expiresAt)
  return (
    <li className="flex items-center gap-2 p-2 bg-surface-2 border border-border-default rounded-lg">
      <KeyRound
        size={13}
        aria-hidden="true"
        className={clsx('mx-1.5 shrink-0', expired ? 'text-text-tertiary' : 'text-brand-400')}
      />
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-1.5 min-w-0 text-xs text-text-primary">
          <span className="truncate">{name}</span>
          {cred.secretHint && (
            <span
              className="shrink-0 font-mono text-text-tertiary"
              title={t('ciCredentials.hint', {hint: cred.secretHint})}
            >
              …{cred.secretHint}
            </span>
          )}
          {expired && <Badge tone="error">{t('ciCredentials.expired')}</Badge>}
        </div>
        <div className="text-2xs text-text-tertiary truncate">
          <span title={absoluteTime(cred.createdAt)}>
            {t('ciCredentials.createdBy', {name: creator, when: relativeTimeText(cred.createdAt)})}
          </span>
          {' · '}
          {cred.expiresAt ? (
            <span title={absoluteTime(cred.expiresAt)}>
              {t(expired ? 'ciCredentials.expiredOn' : 'ciCredentials.expiresOn', {
                date: new Date(cred.expiresAt).toLocaleDateString(),
              })}
            </span>
          ) : (
            t('ciCredentials.neverExpires')
          )}
          {' · '}
          {cred.lastUsedAt ? (
            <span title={absoluteTime(cred.lastUsedAt)}>
              {t('ciCredentials.lastUsed', {when: relativeTimeText(cred.lastUsedAt)})}
            </span>
          ) : (
            t('ciCredentials.neverUsed')
          )}
        </div>
      </div>
      <button
        onClick={() => onRevoke(cred)}
        title={t('ciCredentials.revokeTitle')}
        aria-label={t('ciCredentials.revokeAria', {name})}
        className="p-1.5 rounded text-text-tertiary hover:text-red-400 hover:bg-surface-3"
      >
        <Trash2 size={12} />
      </button>
    </li>
  )
}

function CreateDialog({orgId, onCreated, onClose}: {orgId: string; onCreated: () => void; onClose: () => void}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const formId = useId()
  const nameId = useId()
  const expiryId = useId()
  const [name, setName] = useState('')
  const [expiry, setExpiry] = useState('')
  const [busy, setBusy] = useState(false)
  const [created, setCreated] = useState<{credential: CreatedOrgCICredential; endpoint: string} | null>(null)

  const v = useFormValidation<'name' | 'expiry'>({
    fields: ['name', 'expiry'],
    validate: () => {
      const errors: FieldErrors<'name' | 'expiry'> = {}
      if (!name.trim()) errors.name = t('errors.required', {ns: 'common'})
      if (expiry.trim() !== '') {
        const days = Number(expiry)
        if (!Number.isInteger(days) || days < 1 || days > CI_CREDENTIAL_MAX_EXPIRY_DAYS)
          errors.expiry = t('ciCredentials.expiryInvalid', {max: CI_CREDENTIAL_MAX_EXPIRY_DAYS})
      }
      return errors
    },
  })

  const close = () => {
    if (!busy) onClose()
  }

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const trimmed = name.trim()
    if (!trimmed || busy) return
    if (v.submitValidate()) return
    let expiresInDays: number | undefined
    if (expiry.trim() !== '') expiresInDays = Number(expiry)
    setBusy(true)
    try {
      const credential = await ciCredentialsApi.create(orgId, {
        name: trimmed,
        ...(expiresInDays ? {expiresInDays} : {}),
      })
      setCreated({credential, endpoint: await apiOrigin()})
      onCreated()
    } catch (err) {
      logger.warn(err)
      if (err instanceof ApiError && (err.code === 'CI_CREDENTIAL_LIMIT' || err.status === 409)) {
        toast.error(t('ciCredentials.limitReached'), {description: t('ciCredentials.limitReachedHint')})
      } else if (err instanceof ApiError && err.code === 'CI_CREDENTIALS_UNAVAILABLE') {
        toast.error(t('ciCredentials.encryptionOff'), {description: t('ciCredentials.encryptionOffHint')})
      } else {
        toast.error(t('ciCredentials.createFailed'), {description: getErrorMessage(err)})
      }
    } finally {
      setBusy(false)
    }
  }

  if (created) {
    const command = `PAD_CI_WEBHOOK_SECRET=${created.credential.credential} bakicli report -endpoint ${created.endpoint} -flow-id ${t('ciCredentials.flowIdPlaceholder')}`
    return (
      <Modal
        isOpen
        onClose={onClose}
        title={t('ciCredentials.revealTitle')}
        size="lg"
        fullScreenBelow="sm"
        closeOnBackdrop={false}
        footer={
          <Button size="sm" variant="primary" onClick={onClose}>
            {t('ciCredentials.done')}
          </Button>
        }
      >
        <div className="space-y-4">
          <div className="flex items-start gap-2 p-3 border border-semantic-warning/40 bg-semantic-warning/5 rounded-lg">
            <AlertTriangle size={16} aria-hidden="true" className="text-semantic-warning shrink-0 mt-0.5" />
            <p className="text-sm text-text-primary">{t('ciCredentials.revealWarning')}</p>
          </div>
          <CopyBlock
            label={t('ciCredentials.credentialLabel')}
            value={created.credential.credential}
            copyLabel={t('ciCredentials.copy')}
            copiedToast={t('ciCredentials.credentialCopied')}
            autoFocus
          />
          <CopyBlock
            label={t('ciCredentials.snippetLabel')}
            value={command}
            copyLabel={t('ciCredentials.copyCommand')}
            copiedToast={t('ciCredentials.commandCopied')}
            hint={t('ciCredentials.snippetHint', {placeholder: t('ciCredentials.flowIdPlaceholder')})}
          />
        </div>
      </Modal>
    )
  }

  return (
    <Modal
      isOpen
      onClose={close}
      title={t('ciCredentials.createTitle')}
      description={t('ciCredentials.createDescription')}
      size="md"
      fullScreenBelow="sm"
      closeOnBackdrop={false}
      footer={
        <>
          <Button size="sm" variant="secondary" onClick={close} disabled={busy}>
            {t('ciCredentials.cancel')}
          </Button>
          <Button size="sm" variant="primary" type="submit" form={formId} loading={busy} disabled={!name.trim()}>
            {t('ciCredentials.create')}
          </Button>
        </>
      }
    >
      <form id={formId} onSubmit={e => void handleSubmit(e)} noValidate className="space-y-3">
        <FormField id={nameId} label={t('ciCredentials.nameLabel')}>
          <Input
            id={nameId}
            autoFocus
            autoComplete="off"
            value={name}
            maxLength={CI_CREDENTIAL_MAX_NAME}
            onChange={e => setName(e.target.value)}
            placeholder={t('ciCredentials.namePlaceholder')}
          />
        </FormField>
        <FormField id={expiryId} label={t('ciCredentials.expiryLabel')}>
          <Input
            id={expiryId}
            type="number"
            inputMode="numeric"
            min={1}
            max={CI_CREDENTIAL_MAX_EXPIRY_DAYS}
            step={1}
            value={expiry}
            onChange={e => setExpiry(e.target.value)}
            placeholder={t('ciCredentials.expiryPlaceholder')}
            error={v.errorFor('expiry')}
            hint={v.errorFor('expiry') ? undefined : t('ciCredentials.expiryHint', {max: CI_CREDENTIAL_MAX_EXPIRY_DAYS})}
            onBlur={() => v.touch('expiry')}
          />
        </FormField>
      </form>
    </Modal>
  )
}

function CopyBlock({
  label,
  value,
  copyLabel,
  copiedToast,
  hint,
  autoFocus,
}: {
  label: string
  value: string
  copyLabel: string
  copiedToast: string
  hint?: string
  autoFocus?: boolean
}) {
  const {t} = useTranslation('settings')
  const toast = useToast()
  const [copied, setCopied] = useState(false)

  const copy = async () => {
    try {
      await writeClipboard(value)
      setCopied(true)
      toast.success(copiedToast)
    } catch {
      toast.error(t('ciCredentials.copyFailed'))
    }
  }

  return (
    <div className="space-y-1">
      <p className="text-xs font-medium text-text-secondary">{label}</p>
      <div className="flex items-start gap-2">
        <code className="flex-1 min-w-0 px-3 py-2 rounded-lg bg-surface-2 border border-border-default text-xs font-mono text-text-primary break-all select-all">
          {value}
        </code>
        <Button
          size="sm"
          variant="secondary"
          icon={copied ? Check : Copy}
          onClick={() => void copy()}
          autoFocus={autoFocus}
        >
          {copied ? t('ciCredentials.copied') : copyLabel}
        </Button>
      </div>
      {hint && <p className="text-xs text-text-tertiary">{hint}</p>}
    </div>
  )
}
