import type {Dispatch, SetStateAction} from 'react'
import {useId} from 'react'
import {useTranslation} from 'react-i18next'
import {UserPlus, X, Link2, Copy, Check} from 'lucide-react'
import {writeClipboard} from '@/lib/clipboard'
import type {Organisation, OrgRole} from '@/stores/orgStore'
import {ROLES, ROLE_LABEL_KEYS} from './helpers'
import PendingInvites from './PendingInvites'

interface InviteSectionProps {
  org: Organisation
  inviteEmail: string
  setInviteEmail: Dispatch<SetStateAction<string>>
  inviteError: string | null
  setInviteError: Dispatch<SetStateAction<string | null>>
  inviteRole: OrgRole
  setInviteRole: Dispatch<SetStateAction<OrgRole>>
  inviteBusy: boolean
  handleInvite: () => Promise<void>
  inviteLink: {email: string; url: string} | null
  setInviteLink: Dispatch<SetStateAction<{email: string; url: string} | null>>
  inviteLinkCopied: boolean
  setInviteLinkCopied: Dispatch<SetStateAction<boolean>>
  toastError: (title: string) => void
}

export default function InviteSection({
  org,
  inviteEmail,
  setInviteEmail,
  inviteError,
  setInviteError,
  inviteRole,
  setInviteRole,
  inviteBusy,
  handleInvite,
  inviteLink,
  setInviteLink,
  inviteLinkCopied,
  setInviteLinkCopied,
  toastError,
}: InviteSectionProps) {
  const {t} = useTranslation('settings')
  const errorId = useId()
  return (
    <div>
      <h4 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-2">
        {t('orgs.invite')}
      </h4>
      <div className="flex gap-2">
        <input
          value={inviteEmail}
          onChange={e => {
            setInviteEmail(e.target.value)
            if (inviteError) setInviteError(null)
          }}
          onKeyDown={e => {
            if (e.key === 'Enter') void handleInvite()
          }}
          placeholder={t('orgs.inviteEmailPlaceholder')}
          type="email"
          aria-label={t('orgs.inviteEmailPlaceholder')}
          aria-invalid={!!inviteError}
          aria-describedby={inviteError ? errorId : undefined}
          className={
            'flex-1 px-3 py-1.5 rounded-lg bg-surface-2 border text-sm focus:outline-hidden focus:border-brand-500 ' +
            (inviteError ? 'border-semantic-error' : 'border-border-default')
          }
        />
        <select
          value={inviteRole}
          onChange={e => setInviteRole(e.target.value as OrgRole)}
          className="px-2 py-1.5 rounded-lg bg-surface-2 border border-border-default text-sm focus:outline-hidden focus:border-brand-500"
        >
          {ROLES.map(r => (
            <option key={r} value={r}>
              {t(ROLE_LABEL_KEYS[r])}
            </option>
          ))}
        </select>
        <button
          onClick={handleInvite}
          disabled={!inviteEmail.trim() || inviteBusy}
          className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-brand-500 hover:bg-brand-600 disabled:opacity-50 text-brand-foreground text-sm font-medium"
        >
          <UserPlus size={14} />
          {inviteBusy ? t('orgs.inviting') : t('orgs.invite')}
        </button>
      </div>
      {inviteError && (
        <p id={errorId} className="mt-1 text-2xs text-semantic-error" role="alert">
          {inviteError}
        </p>
      )}
      {inviteLink && (
        <div
          role="status"
          className="mt-2 p-3 rounded-lg border border-semantic-warning/40 bg-semantic-warning/5 space-y-2"
        >
          <p className="text-2xs text-text-primary flex items-center gap-1">
            <Link2 size={12} />
            {t('orgs.inviteLinkBody', {email: inviteLink.email})}
          </p>
          <div className="flex items-center gap-2">
            <code className="flex-1 px-2 py-1.5 rounded-md bg-surface-2 text-2xs text-text-primary break-all select-all">
              {inviteLink.url}
            </code>
            <button
              type="button"
              onClick={async () => {
                try {
                  await writeClipboard(inviteLink.url)
                  setInviteLinkCopied(true)
                } catch {
                  toastError(t('orgs.inviteLinkCopyFailed'))
                }
              }}
              className="flex items-center gap-1 px-2 py-1 text-2xs font-medium rounded-md bg-surface-3 hover:bg-surface-2 text-text-primary"
            >
              {inviteLinkCopied ? <Check size={12} /> : <Copy size={12} />}
              {inviteLinkCopied ? t('orgs.inviteLinkCopied') : t('orgs.inviteLinkCopy')}
            </button>
            <button
              type="button"
              onClick={() => setInviteLink(null)}
              className="p-1 rounded text-text-tertiary hover:text-text-primary"
              aria-label={t('orgs.dismiss')}
            >
              <X size={12} />
            </button>
          </div>
        </div>
      )}

      <PendingInvites orgId={org.id} onInviteSent={handleInvite} />
    </div>
  )
}
