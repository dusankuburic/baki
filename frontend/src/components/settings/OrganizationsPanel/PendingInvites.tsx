import {getErrorMessage} from '@/lib/getErrorMessage'
import {useState} from 'react'
import {useTranslation} from 'react-i18next'
import {X, Clock, Mail} from 'lucide-react'
import clsx from 'clsx'
import {useAsync} from '@/hooks/useAsync'
import {request, ApiError} from '@/api/client'
import {useToast} from '@/components/shared/Toast'
import {useConfirm} from '@/components/shared'
import {roleLabelKey} from './helpers'

interface OrgInvite {
  id: string
  orgId: string
  email: string
  role: string
  invitedBy: string
  expiresAt: string
  acceptedAt?: string
  createdAt: string
}

export default function PendingInvites({orgId}: {orgId: string; onInviteSent?: () => Promise<void>}) {
  const {t} = useTranslation('settings')
  const roleLabel = (role: string) => {
    const key = roleLabelKey(role)
    return key ? t(key) : role
  }
  const {error: toastError, success: toastSuccess} = useToast()
  const {confirm} = useConfirm()

  const {data: invites, refetch} = useAsync<OrgInvite[]>(
    () => request<OrgInvite[]>(`/api/orgs/${orgId}/invites`, {method: 'GET'}).catch(() => []),
    [orgId],
  )
  const inviteList = invites ?? []
  const pending = inviteList.filter(i => !i.acceptedAt)
  const [revokingId, setRevokingId] = useState<string | null>(null)

  const handleRevoke = async (inviteId: string, email: string) => {
    const ok = await confirm({
      title: t('orgs.revokeTitle'),
      message: t('orgs.revokeMessage', {email}),
      danger: true,
      confirmLabel: t('orgs.revokeConfirm'),
    })
    if (!ok) return
    setRevokingId(inviteId)
    try {
      await request(`/api/orgs/${orgId}/invites/${inviteId}`, {method: 'DELETE'})
      toastSuccess(t('orgs.revoked'))
      refetch()
    } catch (err) {
      toastError(
        err instanceof ApiError && err.status === 403
          ? t('orgs.permissionDenied')
          : t('orgs.revokeFailed', {message: getErrorMessage(err, t('orgs.unknownError'))}),
      )
    } finally {
      setRevokingId(null)
    }
  }

  if (pending.length === 0) return null

  const formatDate = (iso: string) => new Date(iso).toLocaleDateString(undefined, {month: 'short', day: 'numeric'})

  return (
    <div className="mt-3">
      <h4 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-2">{t('orgs.pendingInvites')}</h4>
      <div className="border border-border-subtle rounded-lg overflow-hidden">
        {pending.map((inv, j) => {
          const expired = new Date(inv.expiresAt) < new Date()
          return (
            <div
              key={inv.id}
              className={clsx(
                'flex items-center gap-3 p-2.5',
                j !== pending.length - 1 && 'border-b border-border-subtle',
              )}
            >
              <Mail size={14} className="shrink-0 text-text-tertiary" />
              <div className="flex-1 min-w-0">
                <p title={inv.email} className="text-sm text-text-primary truncate">
                  {inv.email}
                </p>
                <p className="text-xs text-text-tertiary flex items-center gap-1">
                  <Clock size={10} />
                  <span>{roleLabel(inv.role)}</span>
                  {' · '}
                  {expired ? (
                    <span className="text-red-400">{t('orgs.expired', {date: formatDate(inv.expiresAt)})}</span>
                  ) : (
                    <>{t('orgs.expires', {date: formatDate(inv.expiresAt)})}</>
                  )}
                </p>
              </div>
              <button
                onClick={() => void handleRevoke(inv.id, inv.email)}
                disabled={revokingId === inv.id}
                className="p-1.5 rounded text-text-tertiary hover:text-semantic-error hover:bg-semantic-error/10 transition-colors disabled:opacity-30"
                title={t('orgs.revokeAria')}
                aria-label={t('orgs.revokeAria')}
              >
                <X size={14} />
              </button>
            </div>
          )
        })}
      </div>
    </div>
  )
}
