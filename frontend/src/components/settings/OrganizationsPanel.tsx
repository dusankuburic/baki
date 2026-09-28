import {useEffect, useState, useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {
  Building2,
  Plus,
  Trash2,
  UserPlus,
  X,
  AlertCircle,
  Clock,
  Mail,
  Link2,
  Copy,
  Check,
  Pencil,
  LogOut,
} from 'lucide-react'
import clsx from 'clsx'
import {useOrgStore, type Organisation, type OrgRole} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {Button, useConfirm} from '@/components/shared'
import OrgChannelsSection from './OrgChannelsSection'
import OrgCustomRulesSection from './OrgCustomRulesSection'
import AzureFoundrySection from './AzureFoundrySection'
import {useAsync} from '@/hooks/useAsync'
import {request} from '@/api/client'
import {writeClipboard} from '@/lib/clipboard'
import {useToast} from '@/components/shared/Toast'

const ROLES: OrgRole[] = ['admin', 'member', 'viewer', 'guest']

// inviteLinkURL builds the same deep link the invite email carries
// (`<base>/#invite=<token>`, see internal/mail/service.go). Shown once after
// creating an invite so an admin can hand it over when email delivery is not
// configured — the server always returned the token, the UI threw it away.
function inviteLinkURL(token: string): string {
  const base = (import.meta.env.BASE_URL || '/').replace(/\/?$/, '/')
  return `${window.location.origin}${base}#invite=${encodeURIComponent(token)}`
}

/**
 * Cloud-only Settings panel that exposes the existing `useOrgStore` actions.
 * Lets the user create/delete orgs, invite/remove members, and change member
 * roles. Gated by `!isTauri()` upstream in SettingsModal.
 */
export default function OrganizationsPanel() {
  const {t} = useTranslation('settings')
  const {error: toastError, success: toastSuccess} = useToast()
  const organisations = useOrgStore(s => s.organisations)
  const activeOrgId = useOrgStore(s => s.activeOrgId)
  const isLoading = useOrgStore(s => s.isLoading)
  const error = useOrgStore(s => s.error)
  const loadOrgs = useOrgStore(s => s.loadOrgs)
  const setActiveOrg = useOrgStore(s => s.setActiveOrg)
  const createOrg = useOrgStore(s => s.createOrg)
  const deleteOrg = useOrgStore(s => s.deleteOrg)
  const renameOrg = useOrgStore(s => s.renameOrg)
  const inviteMember = useOrgStore(s => s.inviteMember)
  const removeMember = useOrgStore(s => s.removeMember)
  const setMemberRole = useOrgStore(s => s.setMemberRole)
  const clearError = useOrgStore(s => s.clearError)
  const currentUser = useAuthStore(s => s.user)
  const {confirm} = useConfirm()

  const [creating, setCreating] = useState(false)
  const [newName, setNewName] = useState('')
  const [createBusy, setCreateBusy] = useState(false)

  const [renamingId, setRenamingId] = useState<string | null>(null)
  const [renameValue, setRenameValue] = useState('')
  const [renameBusy, setRenameBusy] = useState(false)

  const [inviteEmail, setInviteEmail] = useState('')
  const [inviteRole, setInviteRole] = useState<OrgRole>('member')
  const [inviteBusy, setInviteBusy] = useState(false)
  const [inviteError, setInviteError] = useState<string | null>(null)
  const [inviteLink, setInviteLink] = useState<{email: string; url: string} | null>(null)
  const [inviteLinkCopied, setInviteLinkCopied] = useState(false)

  useEffect(() => {
    void loadOrgs()
  }, [loadOrgs])

  const selected = useMemo(() => organisations.find(o => o.id === activeOrgId) ?? null, [organisations, activeOrgId])

  const handleCreate = async () => {
    const name = newName.trim()
    if (!name) return
    setCreateBusy(true)
    try {
      const o = await createOrg(name)
      setActiveOrg(o.id)
      setNewName('')
      setCreating(false)
    } catch {
      // error surfaces via store.error
    } finally {
      setCreateBusy(false)
    }
  }

  const handleInvite = async () => {
    const email = inviteEmail.trim()
    if (!selected || !email) return
    // Client-side format gate (U4.3): a typo'd address used to burn a
    // round-trip and surface a raw server error.
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
      setInviteError(t('orgs.inviteInvalidEmail'))
      return
    }
    setInviteError(null)
    setInviteBusy(true)
    try {
      const token = await inviteMember(selected.id, email, inviteRole)
      setInviteEmail('')
      setInviteLink(token ? {email, url: inviteLinkURL(token)} : null)
      setInviteLinkCopied(false)
    } catch {
      // error surfaces via store.error
    } finally {
      setInviteBusy(false)
    }
  }

  const handleRemoveMember = async (userId: string) => {
    if (!selected) return
    const ok = await confirm({
      title: t('orgs.removeMemberTitle'),
      message: t('orgs.removeMemberMessage'),
      danger: true,
      confirmLabel: t('orgs.removeMemberConfirm'),
    })
    if (!ok) return
    try {
      await removeMember(selected.id, userId)
    } catch {
      // error surfaces via store.error
    }
  }

  // A non-admin member leaves by removing their own row — the server allows
  // exactly that self-removal. removeMember reloads the list, so the org drops
  // out and the active org falls back to Personal.
  const handleLeave = async (org: Organisation) => {
    if (!currentUser) return
    const ok = await confirm({
      title: t('orgs.leaveTitle'),
      message: t('orgs.leaveMessage', {name: org.name}),
      danger: true,
      confirmLabel: t('orgs.leaveConfirm'),
    })
    if (!ok) return
    try {
      await removeMember(org.id, currentUser.id)
      toastSuccess(t('orgs.left', {name: org.name}))
    } catch {
      // error surfaces via store.error
    }
  }

  const startRename = (org: Organisation) => {
    setRenamingId(org.id)
    setRenameValue(org.name)
  }

  const handleRename = async (org: Organisation) => {
    const name = renameValue.trim()
    // Blank or unchanged is a cancel, not a request the server would refuse.
    if (!name || name === org.name) {
      setRenamingId(null)
      return
    }
    setRenameBusy(true)
    try {
      await renameOrg(org.id, name)
      setRenamingId(null)
    } catch {
      // error surfaces via store.error; keep the editor open with the typed name
    } finally {
      setRenameBusy(false)
    }
  }

  const handleRoleChange = async (userId: string, role: OrgRole) => {
    if (!selected) return
    try {
      await setMemberRole(selected.id, userId, role)
    } catch {
      // error surfaces via store.error
    }
  }

  const handleDeleteOrg = async (org: Organisation) => {
    const ok = await confirm({
      title: t('orgs.deleteTitle'),
      message: t('orgs.deleteMessage', {name: org.name}),
      danger: true,
      confirmLabel: t('orgs.deleteConfirm'),
    })
    if (!ok) return
    try {
      await deleteOrg(org.id)
    } catch {
      // error surfaces via store.error
    }
  }

  return (
    <div className="space-y-6">
      {!creating && (
        <div className="flex justify-end">
          <Button variant="primary" icon={Plus} onClick={() => setCreating(true)}>
            {t('orgs.newOrg')}
          </Button>
        </div>
      )}

      {error && (
        <div
          role="alert"
          className="flex items-start gap-2 p-3 rounded-lg bg-semantic-error/10 border border-semantic-error/30 text-semantic-error"
        >
          <AlertCircle size={16} className="shrink-0 mt-0.5" />
          <span className="text-sm flex-1">{error}</span>
          <button onClick={clearError} className="text-xs hover:opacity-80">
            {t('orgs.dismiss')}
          </button>
        </div>
      )}

      {creating && (
        <div className="p-4 border border-border-default rounded-xl bg-surface-1 space-y-3">
          <label className="block text-sm font-medium text-text-primary">{t('orgs.nameLabel')}</label>
          <input
            autoFocus
            value={newName}
            onChange={e => setNewName(e.target.value)}
            onKeyDown={e => {
              if (e.key === 'Enter') void handleCreate()
            }}
            placeholder={t('orgs.namePlaceholder')}
            className="w-full px-3 py-2 rounded-lg bg-surface-2 border border-border-default text-sm focus:outline-none focus:border-brand-500"
          />
          <div className="flex gap-2 justify-end">
            <button
              onClick={() => {
                setCreating(false)
                setNewName('')
              }}
              className="px-3 py-1.5 rounded-lg text-sm text-text-secondary hover:bg-surface-3"
            >
              Cancel
            </button>
            <button
              onClick={handleCreate}
              disabled={!newName.trim() || createBusy}
              className="px-3 py-1.5 rounded-lg bg-brand-500 hover:bg-brand-600 disabled:opacity-50 text-brand-foreground text-sm font-medium"
            >
              {createBusy ? t('orgs.creating') : t('orgs.create')}
            </button>
          </div>
        </div>
      )}

      {isLoading && organisations.length === 0 ? (
        <div className="py-8 text-center text-sm text-text-tertiary">{t('orgs.loading')}</div>
      ) : organisations.length === 0 ? (
        <div className="py-8 text-center">
          <Building2 size={32} className="mx-auto text-text-tertiary/50" />
          <p className="mt-2 text-sm text-text-tertiary">{t('orgs.empty')}</p>
        </div>
      ) : (
        <div className="border border-border-default rounded-xl overflow-hidden bg-surface-1">
          {organisations.map(org => {
            const isOwner = currentUser?.id === org.ownerId
            // The roster comes with the list, but a server that omits it must
            // not take the whole panel down with a nil dereference.
            const members = org.members ?? []
            const isAdmin = members.find(m => m.userId === currentUser?.id)?.role === 'admin'
            const canManageMembers = isOwner || isAdmin
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
                            className="flex-1 min-w-0 px-2 py-1 rounded-md bg-surface-2 border border-border-default text-sm focus:outline-none focus:border-brand-500"
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
                        <span title={org.name} className="text-sm font-semibold text-text-primary truncate">
                          {org.name}
                        </span>
                      )}
                      {isOwner && (
                        <span className="text-2xs font-bold uppercase tracking-wider px-1.5 py-0.5 rounded bg-brand-500/15 text-brand-400">
                          Owner
                        </span>
                      )}
                    </div>
                    <p className="text-xs text-text-tertiary mt-0.5">
                      {members.length} {members.length === 1 ? 'member' : 'members'}
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
                    <OrgChannelsSection orgId={org.id} isAdmin={canManageMembers} />
                    <OrgCustomRulesSection orgId={org.id} isAdmin={canManageMembers} />
                    {/* The org's shared Azure AI Foundry credentials: members
                        read, admins (and the owner) manage. A member's own
                        credentials always take precedence. */}
                    <div>
                      <h4 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-1">
                        {t('providers.azureOrgTitle')}
                      </h4>
                      <p className="text-xs text-text-tertiary mb-3">{t('providers.azureOrgHint')}</p>
                      <AzureFoundrySection scope={{kind: 'org', orgId: org.id, canEdit: canManageMembers}} />
                    </div>
                    <div>
                      <h4 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-2">
                        {t('orgs.members')}
                      </h4>
                      <div className="border border-border-subtle rounded-lg overflow-hidden">
                        {members.length === 0 ? (
                          <div className="p-3 text-xs text-text-tertiary text-center">{t('orgs.noMembers')}</div>
                        ) : (
                          members.map((m, j) => (
                            <div
                              key={m.userId}
                              className={clsx(
                                'flex items-center gap-3 p-2.5',
                                j !== members.length - 1 && 'border-b border-border-subtle',
                              )}
                            >
                              <div className="flex-1 min-w-0">
                                <p
                                  title={m.user?.displayName || m.user?.email || m.userId}
                                  className="text-sm text-text-primary truncate"
                                >
                                  {m.user?.displayName || m.user?.email || m.userId}
                                </p>
                                {m.user?.email && m.user.displayName && (
                                  <p title={m.user.email} className="text-xs text-text-tertiary truncate">
                                    {m.user.email}
                                  </p>
                                )}
                              </div>
                              {canManageMembers ? (
                                <select
                                  value={m.role}
                                  onChange={e => void handleRoleChange(m.userId, e.target.value as OrgRole)}
                                  disabled={m.userId === org.ownerId}
                                  className="px-2 py-1 rounded-md bg-surface-2 border border-border-default text-xs text-text-primary focus:outline-none focus:border-brand-500 disabled:opacity-50"
                                >
                                  {ROLES.map(r => (
                                    <option key={r} value={r}>
                                      {r}
                                    </option>
                                  ))}
                                </select>
                              ) : (
                                <span className="px-2 py-1 text-xs text-text-tertiary capitalize">{m.role}</span>
                              )}
                              {canManageMembers ? (
                                <button
                                  onClick={() => void handleRemoveMember(m.userId)}
                                  disabled={m.userId === org.ownerId}
                                  className="p-1.5 rounded text-text-tertiary hover:text-semantic-error hover:bg-semantic-error/10 transition-colors disabled:opacity-30 disabled:hover:bg-transparent disabled:hover:text-text-tertiary"
                                  title={
                                    m.userId === org.ownerId ? t('orgs.ownerNotRemovable') : t('orgs.removeMemberAria')
                                  }
                                  aria-label={
                                    m.userId === org.ownerId ? t('orgs.ownerNotRemovable') : t('orgs.removeMemberAria')
                                  }
                                >
                                  <X size={14} />
                                </button>
                              ) : m.userId === currentUser?.id ? (
                                <button
                                  onClick={() => void handleLeave(org)}
                                  className="flex items-center gap-1 px-2 py-1 rounded text-xs text-text-tertiary hover:text-semantic-error hover:bg-semantic-error/10 transition-colors"
                                  title={t('orgs.leaveAria')}
                                  aria-label={t('orgs.leaveAria')}
                                >
                                  <LogOut size={12} />
                                  {t('orgs.leave')}
                                </button>
                              ) : null}
                            </div>
                          ))
                        )}
                      </div>
                    </div>

                    {canManageMembers && (
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
                            aria-invalid={!!inviteError}
                            className={
                              'flex-1 px-3 py-1.5 rounded-lg bg-surface-2 border text-sm focus:outline-none focus:border-brand-500 ' +
                              (inviteError ? 'border-semantic-error' : 'border-border-default')
                            }
                          />
                          <select
                            value={inviteRole}
                            onChange={e => setInviteRole(e.target.value as OrgRole)}
                            className="px-2 py-1.5 rounded-lg bg-surface-2 border border-border-default text-sm focus:outline-none focus:border-brand-500"
                          >
                            {ROLES.map(r => (
                              <option key={r} value={r}>
                                {r}
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
                          <p className="mt-1 text-2xs text-semantic-error" role="alert">
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
                    )}
                  </div>
                )}
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}

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

function PendingInvites({orgId}: {orgId: string; onInviteSent?: () => Promise<void>}) {
  const {t} = useTranslation('settings')
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
      toastError(t('orgs.revokeFailed', {message: err instanceof Error ? err.message : 'Unknown error'}))
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
                  <span className="capitalize">{inv.role}</span>
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
