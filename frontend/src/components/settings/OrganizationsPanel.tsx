import {useEffect, useState, useMemo} from 'react'
import {Building2, Plus, Trash2, UserPlus, X, AlertCircle, Clock, Mail} from 'lucide-react'
import clsx from 'clsx'
import {useOrgStore, type Organisation, type OrgRole} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {useConfirm} from '@/components/shared'
import {useAsync} from '@/hooks/useAsync'
import {request} from '@/api/client'
import {useToast} from '@/components/shared/Toast'

const ROLES: OrgRole[] = ['admin', 'member', 'viewer', 'guest']

/**
 * Cloud-only Settings panel that exposes the existing `useOrgStore` actions.
 * Lets the user create/delete orgs, invite/remove members, and change member
 * roles. Gated by `!isTauri()` upstream in SettingsModal.
 */
export default function OrganizationsPanel() {
  const organisations = useOrgStore(s => s.organisations)
  const activeOrgId = useOrgStore(s => s.activeOrgId)
  const isLoading = useOrgStore(s => s.isLoading)
  const error = useOrgStore(s => s.error)
  const loadOrgs = useOrgStore(s => s.loadOrgs)
  const setActiveOrg = useOrgStore(s => s.setActiveOrg)
  const createOrg = useOrgStore(s => s.createOrg)
  const deleteOrg = useOrgStore(s => s.deleteOrg)
  const inviteMember = useOrgStore(s => s.inviteMember)
  const removeMember = useOrgStore(s => s.removeMember)
  const setMemberRole = useOrgStore(s => s.setMemberRole)
  const clearError = useOrgStore(s => s.clearError)
  const currentUser = useAuthStore(s => s.user)
  const {confirm} = useConfirm()

  const [creating, setCreating] = useState(false)
  const [newName, setNewName] = useState('')
  const [createBusy, setCreateBusy] = useState(false)

  const [inviteEmail, setInviteEmail] = useState('')
  const [inviteRole, setInviteRole] = useState<OrgRole>('member')
  const [inviteBusy, setInviteBusy] = useState(false)

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
    setInviteBusy(true)
    try {
      await inviteMember(selected.id, email, inviteRole)
      setInviteEmail('')
    } catch {
      // error surfaces via store.error
    } finally {
      setInviteBusy(false)
    }
  }

  const handleRemoveMember = async (userId: string) => {
    if (!selected) return
    const ok = await confirm({
      title: 'Remove member',
      message: 'Remove this member from the organization?',
      danger: true,
      confirmLabel: 'Remove',
    })
    if (!ok) return
    try {
      await removeMember(selected.id, userId)
    } catch {
      // error surfaces via store.error
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
      title: 'Delete organization',
      message: `Delete organization "${org.name}"? This cannot be undone.`,
      danger: true,
      confirmLabel: 'Delete',
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
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-xl font-semibold text-text-primary flex items-center gap-2">
            <Building2 size={20} className="text-brand-500" />
            Organizations
          </h2>
          <p className="text-sm text-text-secondary mt-1">
            Group flows and share them with teammates. Roles control who can view or edit.
          </p>
        </div>
        {!creating && (
          <button
            onClick={() => setCreating(true)}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-brand-500 hover:bg-brand-600 text-brand-foreground text-sm font-medium transition-colors shrink-0"
          >
            <Plus size={14} />
            New organization
          </button>
        )}
      </div>

      {error && (
        <div role="alert" className="flex items-start gap-2 p-3 rounded-lg bg-semantic-error/10 border border-semantic-error/30 text-semantic-error">
          <AlertCircle size={16} className="shrink-0 mt-0.5" />
          <span className="text-sm flex-1">{error}</span>
          <button onClick={clearError} className="text-xs hover:opacity-80">
            Dismiss
          </button>
        </div>
      )}

      {creating && (
        <div className="p-4 border border-border-default rounded-xl bg-surface-1 space-y-3">
          <label className="block text-sm font-medium text-text-primary">Organization name</label>
          <input
            autoFocus
            value={newName}
            onChange={e => setNewName(e.target.value)}
            onKeyDown={e => {
              if (e.key === 'Enter') void handleCreate()
            }}
            placeholder="e.g. Acme RPA"
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
              {createBusy ? 'Creating…' : 'Create'}
            </button>
          </div>
        </div>
      )}

      {isLoading && organisations.length === 0 ? (
        <div className="py-8 text-center text-sm text-text-tertiary">Loading…</div>
      ) : organisations.length === 0 ? (
        <div className="py-8 text-center">
          <Building2 size={32} className="mx-auto text-text-tertiary/50" />
          <p className="mt-2 text-sm text-text-tertiary">No organizations yet.</p>
        </div>
      ) : (
        <div className="border border-border-default rounded-xl overflow-hidden bg-surface-1">
          {organisations.map(org => {
            const isOwner = currentUser?.id === org.ownerId
            const isAdmin = org.members.find(m => m.userId === currentUser?.id)?.role === 'admin'
            const canManageMembers = isOwner || isAdmin
            const isActive = org.id === activeOrgId
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
                      <span title={org.name} className="text-sm font-semibold text-text-primary truncate">
                        {org.name}
                      </span>
                      {isOwner && (
                        <span className="text-2xs font-bold uppercase tracking-wider px-1.5 py-0.5 rounded bg-brand-500/15 text-brand-400">
                          Owner
                        </span>
                      )}
                    </div>
                    <p className="text-xs text-text-tertiary mt-0.5">
                      {org.members.length} {org.members.length === 1 ? 'member' : 'members'}
                    </p>
                  </div>
                  {isOwner && (
                    <button
                      onClick={e => {
                        e.stopPropagation()
                        void handleDeleteOrg(org)
                      }}
                      className="p-1.5 rounded text-text-tertiary hover:text-semantic-error hover:bg-semantic-error/10 transition-colors"
                      title="Delete organization"
                      aria-label="Delete organization"
                    >
                      <Trash2 size={14} />
                    </button>
                  )}
                </div>

                {isActive && selected?.id === org.id && (
                  <div className="p-4 border-t border-border-subtle space-y-4 bg-surface-1/50">
                    <div>
                      <h4 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-2">Members</h4>
                      <div className="border border-border-subtle rounded-lg overflow-hidden">
                        {org.members.length === 0 ? (
                          <div className="p-3 text-xs text-text-tertiary text-center">No members yet.</div>
                        ) : (
                          org.members.map((m, j) => (
                            <div
                              key={m.userId}
                              className={clsx(
                                'flex items-center gap-3 p-2.5',
                                j !== org.members.length - 1 && 'border-b border-border-subtle',
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
                                  title={m.userId === org.ownerId ? 'Owner cannot be removed' : 'Remove member'}
                                  aria-label={m.userId === org.ownerId ? 'Owner cannot be removed' : 'Remove member'}
                                >
                                  <X size={14} />
                                </button>
                              ) : null}
                            </div>
                          ))
                        )}
                      </div>
                    </div>

                    {canManageMembers && (
                      <div>
                        <h4 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-2">Invite</h4>
                        <div className="flex gap-2">
                          <input
                            value={inviteEmail}
                            onChange={e => setInviteEmail(e.target.value)}
                            onKeyDown={e => {
                              if (e.key === 'Enter') void handleInvite()
                            }}
                            placeholder="teammate@example.com"
                            type="email"
                            className="flex-1 px-3 py-1.5 rounded-lg bg-surface-2 border border-border-default text-sm focus:outline-none focus:border-brand-500"
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
                            {inviteBusy ? 'Inviting…' : 'Invite'}
                          </button>
                        </div>

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
  const {error: toastError, success: toastSuccess} = useToast()
  const {confirm} = useConfirm()

  const {
    data: invites,
    refetch,
  } = useAsync<OrgInvite[]>(
    () => request<OrgInvite[]>(`/api/orgs/${orgId}/invites`, {method: 'GET'}).catch(() => []),
    [orgId],
  )
  const inviteList = invites ?? []
  const pending = inviteList.filter(i => !i.acceptedAt)
  const [revokingId, setRevokingId] = useState<string | null>(null)

  const handleRevoke = async (inviteId: string, email: string) => {
    const ok = await confirm({
      title: 'Revoke invite',
      message: `Revoke the pending invite for ${email}? They will no longer be able to accept it.`,
      danger: true,
      confirmLabel: 'Revoke',
    })
    if (!ok) return
    setRevokingId(inviteId)
    try {
      await request(`/api/orgs/${orgId}/invites/${inviteId}`, {method: 'DELETE'})
      toastSuccess('Invite revoked')
      refetch()
    } catch (err) {
      toastError('Failed to revoke invite: ' + (err instanceof Error ? err.message : 'Unknown error'))
    } finally {
      setRevokingId(null)
    }
  }

  if (pending.length === 0) return null

  const formatDate = (iso: string) =>
    new Date(iso).toLocaleDateString(undefined, {month: 'short', day: 'numeric'})

  return (
    <div className="mt-3">
      <h4 className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-2">Pending Invites</h4>
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
                <p title={inv.email} className="text-sm text-text-primary truncate">{inv.email}</p>
                <p className="text-xs text-text-tertiary flex items-center gap-1">
                  <Clock size={10} />
                  <span className="capitalize">{inv.role}</span>
                  {' · '}
                  {expired ? (
                    <span className="text-red-400">Expired {formatDate(inv.expiresAt)}</span>
                  ) : (
                    <>Expires {formatDate(inv.expiresAt)}</>
                  )}
                </p>
              </div>
              <button
                onClick={() => void handleRevoke(inv.id, inv.email)}
                disabled={revokingId === inv.id}
                className="p-1.5 rounded text-text-tertiary hover:text-semantic-error hover:bg-semantic-error/10 transition-colors disabled:opacity-30"
                title="Revoke invite"
                aria-label="Revoke invite"
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
