import type {Dispatch, SetStateAction} from 'react'
import {useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Pencil, Trash2} from 'lucide-react'
import clsx from 'clsx'
import type {AuthUser} from '@/api/auth'
import type {Organisation, OrgRole} from '@/stores/orgStore'
import {Badge} from '@/components/shared'
import OrgAIProvidersSection from '../OrgAIProvidersSection'
import OrgChannelsSection from '../OrgChannelsSection'
import OrgCICredentialsSection from '../OrgCICredentialsSection'
import OrgCustomRulesSection from '../OrgCustomRulesSection'
import {orgNameId} from './helpers'
import OrgAzureSection from './OrgAzureSection'
import MembersSection from './MembersSection'
import InviteSection from './InviteSection'

interface OrgListSectionProps {
  organisations: Organisation[]
  activeOrgId: string | null
  selected: Organisation | null
  currentUser: AuthUser | null
  setActiveOrg: (id: string | null) => void
  setForbidden: Dispatch<SetStateAction<boolean>>
  reportFailure: (err: unknown) => void
  renameOrg: (orgId: string, name: string) => Promise<void>
  handleDeleteOrg: (org: Organisation) => Promise<void>
  handleLeave: (org: Organisation) => Promise<void>
  handleRemoveMember: (userId: string, name?: string) => Promise<void>
  handleRoleChange: (userId: string, role: OrgRole, name?: string) => Promise<void>
  orgAzureUnavailable: boolean
  credentialDisabledNote: {orgId: string; name: string} | null
  markOrgAzureUnavailable: () => void
  handleOrgAzureConnectionSaved: () => void
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

export default function OrgListSection({
  organisations,
  activeOrgId,
  selected,
  currentUser,
  setActiveOrg,
  setForbidden,
  reportFailure,
  renameOrg,
  handleDeleteOrg,
  handleLeave,
  handleRemoveMember,
  handleRoleChange,
  orgAzureUnavailable,
  credentialDisabledNote,
  markOrgAzureUnavailable,
  handleOrgAzureConnectionSaved,
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
}: OrgListSectionProps) {
  const {t} = useTranslation('settings')
  const [renamingId, setRenamingId] = useState<string | null>(null)
  const [renameValue, setRenameValue] = useState('')
  const [renameBusy, setRenameBusy] = useState(false)

  const startRename = (org: Organisation) => {
    setRenamingId(org.id)
    setRenameValue(org.name)
  }

  const handleRename = async (org: Organisation) => {
    const name = renameValue.trim()
    if (!name || name === org.name) {
      setRenamingId(null)
      return
    }
    setRenameBusy(true)
    setForbidden(false)
    try {
      await renameOrg(org.id, name)
      setRenamingId(null)
    } catch (err) {
      reportFailure(err)
    } finally {
      setRenameBusy(false)
    }
  }

  return (
    <div className="border border-border-default rounded-xl overflow-hidden bg-surface-1">
      {organisations.map(org => {
        const isOwner = currentUser?.id === org.ownerId
        const members = org.members ?? []
        const canManageMembers = members.find(m => m.userId === currentUser?.id)?.role === 'admin'
        const isActive = org.id === activeOrgId
        const isRenaming = renamingId === org.id
        return (
          <div
            key={org.id}
            className={clsx(
              'border-b border-border-subtle last:border-b-0 transition-colors',
              isActive ? 'bg-surface-2' : 'hover:bg-surface-2/50',
            )}
          >
            <div
              onClick={() => setActiveOrg(isActive ? null : org.id)}
              className="flex items-center justify-between p-4 cursor-pointer"
            >
              <div className="flex-1 min-w-0">
                <div className="flex items-center gap-2">
                  {isRenaming ? (
                    <div className="flex items-center gap-2 flex-1 min-w-0" onClick={e => e.stopPropagation()}>
                      <input
                        autoFocus
                        value={renameValue}
                        onChange={e => setRenameValue(e.target.value)}
                        onKeyDown={e => {
                          if (e.key === 'Enter') void handleRename(org)
                          if (e.key === 'Escape') setRenamingId(null)
                        }}
                        aria-label={t('orgs.renameInputAria')}
                        className="flex-1 min-w-0 px-2 py-1 rounded-md bg-surface-2 border border-border-default text-sm focus:outline-hidden focus:border-brand-500"
                      />
                      <button
                        onClick={() => void handleRename(org)}
                        disabled={renameBusy || !renameValue.trim()}
                        className="px-2 py-1 rounded-md bg-brand-500 hover:bg-brand-600 disabled:opacity-50 text-brand-foreground text-xs font-medium"
                      >
                        {t('orgs.renameSave')}
                      </button>
                      <button
                        onClick={() => setRenamingId(null)}
                        className="px-2 py-1 rounded-md text-xs text-text-secondary hover:bg-surface-3"
                      >
                        {t('orgs.cancel')}
                      </button>
                    </div>
                  ) : (
                    <span
                      id={orgNameId(org.id)}
                      tabIndex={-1}
                      title={org.name}
                      className="text-sm font-semibold text-text-primary truncate"
                    >
                      {org.name}
                    </span>
                  )}
                  {isOwner && <Badge tone="brand">{t('orgs.owner')}</Badge>}
                </div>
                <p className="text-xs text-text-tertiary mt-0.5">
                  {t('orgs.memberCount', {count: members.length})}
                </p>
              </div>
              {canManageMembers && !isRenaming && (
                <button
                  onClick={e => {
                    e.stopPropagation()
                    startRename(org)
                  }}
                  className="p-1.5 rounded text-text-tertiary hover:text-text-primary hover:bg-surface-3 transition-colors"
                  title={t('orgs.renameAria')}
                  aria-label={t('orgs.renameAria')}
                >
                  <Pencil size={14} />
                </button>
              )}
              {isOwner && (
                <button
                  onClick={e => {
                    e.stopPropagation()
                    void handleDeleteOrg(org)
                  }}
                  className="p-1.5 rounded text-text-tertiary hover:text-semantic-error hover:bg-semantic-error/10 transition-colors"
                  title={t('orgs.deleteAria')}
                  aria-label={t('orgs.deleteAria')}
                >
                  <Trash2 size={14} />
                </button>
              )}
            </div>

            {isActive && selected?.id === org.id && (
              <div className="p-4 border-t border-border-subtle space-y-4 bg-surface-1/50">
                <OrgChannelsSection orgId={org.id} isAdmin={canManageMembers} members={members} />
                {canManageMembers && <OrgCICredentialsSection orgId={org.id} members={members} />}
                {canManageMembers && <OrgAIProvidersSection orgId={org.id} />}
                <OrgCustomRulesSection orgId={org.id} isAdmin={canManageMembers} />
                {!orgAzureUnavailable && (
                  <OrgAzureSection
                    org={org}
                    currentUser={currentUser}
                    canManageMembers={canManageMembers}
                    credentialDisabledNote={credentialDisabledNote}
                    handleOrgAzureConnectionSaved={handleOrgAzureConnectionSaved}
                    markOrgAzureUnavailable={markOrgAzureUnavailable}
                  />
                )}
                <MembersSection
                  org={org}
                  members={members}
                  currentUser={currentUser}
                  canManageMembers={canManageMembers}
                  handleRoleChange={handleRoleChange}
                  handleRemoveMember={handleRemoveMember}
                  handleLeave={handleLeave}
                />
                {canManageMembers && (
                  <InviteSection
                    org={org}
                    inviteEmail={inviteEmail}
                    setInviteEmail={setInviteEmail}
                    inviteError={inviteError}
                    setInviteError={setInviteError}
                    inviteRole={inviteRole}
                    setInviteRole={setInviteRole}
                    inviteBusy={inviteBusy}
                    handleInvite={handleInvite}
                    inviteLink={inviteLink}
                    setInviteLink={setInviteLink}
                    inviteLinkCopied={inviteLinkCopied}
                    setInviteLinkCopied={setInviteLinkCopied}
                    toastError={toastError}
                  />
                )}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}
