import {useCallback, useEffect, useState, useMemo} from 'react'
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
import {useChatStore} from '@/stores/chatStore'
import {useAzureFoundryStore} from '@/stores/azureFoundryStore'
import {useUIStore} from '@/stores/uiStore'
import {Badge, Button, useConfirm} from '@/components/shared'
import OrgChannelsSection from './OrgChannelsSection'
import OrgCustomRulesSection from './OrgCustomRulesSection'
import AzureFoundryPanel from './azure/AzureFoundryPanel'
import {orgAzureAnchor, orgIdFromAnchor} from './azure/registryView'
import {useAsync} from '@/hooks/useAsync'
import {request, ApiError} from '@/api/client'
import {writeClipboard} from '@/lib/clipboard'
import {useToast} from '@/components/shared/Toast'

const ROLES: OrgRole[] = ['admin', 'member', 'viewer', 'guest']

// The i18n key naming each role. `satisfies` keeps it in step with OrgRole.
const ROLE_LABEL_KEYS = {
  admin: 'orgs.roleAdmin',
  member: 'orgs.roleMember',
  viewer: 'orgs.roleViewer',
  guest: 'orgs.roleGuest',
} as const satisfies Record<OrgRole, string>

// roleLabelKey tolerates a role this client does not know (a newer server):
// null, and the caller shows the raw value.
function roleLabelKey(role: string): (typeof ROLE_LABEL_KEYS)[OrgRole] | null {
  return Object.prototype.hasOwnProperty.call(ROLE_LABEL_KEYS, role) ? ROLE_LABEL_KEYS[role as OrgRole] : null
}

// The organisation's name in its row: a deep link's landing spot when the
// organisation is not the one expanded.
function orgNameId(orgId: string): string {
  return `org-name-${orgId}`
}

// Admins and members chat with the organisation's shared Azure AI Foundry
// models; viewers and guests may not (the server refuses them the listing).
function canUseOrgModels(org: Organisation, userId: string | undefined): boolean {
  const role = (org.members ?? []).find(m => m.userId === userId)?.role
  return role === 'admin' || role === 'member'
}

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
  const roleLabel = (role: string) => {
    const key = roleLabelKey(role)
    return key ? t(key) : role
  }
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
  const bumpProviderEpoch = useChatStore(s => s.bumpProviderEpoch)
  const {confirm} = useConfirm()

  // A 403 from an org action (the caller's role changed since the list
  // loaded) shows localized permission copy instead of the server's raw
  // "forbidden"; cleared when the next action starts.
  const [forbidden, setForbidden] = useState(false)
  const reportFailure = (err: unknown) => {
    if (err instanceof ApiError && err.status === 403) {
      clearError()
      setForbidden(true)
    }
  }
  const dismissError = () => {
    clearError()
    setForbidden(false)
  }
  const bannerText = forbidden ? t('orgs.permissionDenied') : error

  // Set when the deployment has no org keystore: the shared-models block
  // hides for every org (it is a deployment property, not an org's).
  const [orgAzureUnavailable, setOrgAzureUnavailable] = useState(false)
  const markOrgAzureUnavailable = useCallback(() => setOrgAzureUnavailable(true), [])
  // Removing or demoting the lender of an org resource's credential
  // disables it server-side. The note says what disappeared and who
  // re-enters it — in that org only — and the registry is marked stale, so
  // the block (and the members' "Shared with you") fetch it again and show
  // the resource as needing credentials. The chat picker refetches too.
  const [credentialDisabledNote, setCredentialDisabledNote] = useState<{orgId: string; name: string} | null>(null)
  const reportCredentialDisabled = useCallback(
    (orgId: string, name: string) => {
      setCredentialDisabledNote({orgId, name})
      const azure = useAzureFoundryStore.getState()
      azure.invalidate({kind: 'org', orgId})
      azure.invalidate({kind: 'user'})
      bumpProviderEpoch()
    },
    [bumpProviderEpoch],
  )
  // Credentials entered again (or a resource added): the note is stale. The
  // registry store already relisted the chat picker.
  const handleOrgAzureConnectionSaved = useCallback(() => setCredentialDisabledNote(null), [])

  // A Settings deep link to an organisation's Azure AI Foundry block
  // ("Manage in Organizations"). The active organisation's block focuses its
  // own heading once loaded; any other organisation — not switched to here,
  // which would reset the open flow and chat — gets its name focused, so the
  // user lands on it. Spent either way.
  const settingsAnchor = useUIStore(s => s.settingsAnchor)
  useEffect(() => {
    const anchoredOrg = orgIdFromAnchor(settingsAnchor)
    if (!anchoredOrg || isLoading) return
    const org = organisations.find(o => o.id === anchoredOrg)
    if (org && org.id === activeOrgId && !orgAzureUnavailable && canUseOrgModels(org, currentUser?.id)) return
    if (org) document.getElementById(orgNameId(org.id))?.focus()
    useUIStore.getState().clearSettingsAnchor()
  }, [settingsAnchor, isLoading, organisations, activeOrgId, orgAzureUnavailable, currentUser?.id])

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
    setForbidden(false)
    try {
      const o = await createOrg(name)
      setActiveOrg(o.id)
      setNewName('')
      setCreating(false)
    } catch (err) {
      reportFailure(err)
      // other errors surface via store.error
    } finally {
      setCreateBusy(false)
    }
  }

  const handleInvite = async () => {
    const email = inviteEmail.trim()
    if (!selected || !email) return
    // Client-side format gate: a typo'd address used to burn a
    // round-trip and surface a raw server error.
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
      setInviteError(t('orgs.inviteInvalidEmail'))
      return
    }
    setInviteError(null)
    setInviteBusy(true)
    setForbidden(false)
    try {
      const token = await inviteMember(selected.id, email, inviteRole)
      setInviteEmail('')
      setInviteLink(token ? {email, url: inviteLinkURL(token)} : null)
      setInviteLinkCopied(false)
    } catch (err) {
      reportFailure(err)
      // other errors surface via store.error
    } finally {
      setInviteBusy(false)
    }
  }

  const handleRemoveMember = async (userId: string, name?: string) => {
    if (!selected) return
    const ok = await confirm({
      title: t('orgs.removeMemberTitle'),
      message: t('orgs.removeMemberMessage'),
      danger: true,
      confirmLabel: t('orgs.removeMemberConfirm'),
    })
    if (!ok) return
    setForbidden(false)
    try {
      const reply = await removeMember(selected.id, userId)
      if (reply?.sharedCredentialDisabled) reportCredentialDisabled(selected.id, name ?? userId)
    } catch (err) {
      reportFailure(err)
      // other errors surface via store.error
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
    setForbidden(false)
    try {
      await removeMember(org.id, currentUser.id)
      toastSuccess(t('orgs.left', {name: org.name}))
    } catch (err) {
      reportFailure(err)
      // other errors surface via store.error
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
    setForbidden(false)
    try {
      await renameOrg(org.id, name)
      setRenamingId(null)
    } catch (err) {
      reportFailure(err)
      // other errors surface via store.error; keep the editor open with the typed name
    } finally {
      setRenameBusy(false)
    }
  }

  const handleRoleChange = async (userId: string, role: OrgRole, name?: string) => {
    if (!selected) return
    setForbidden(false)
    try {
      const reply = await setMemberRole(selected.id, userId, role)
      if (reply?.sharedCredentialDisabled) reportCredentialDisabled(selected.id, name ?? userId)
    } catch (err) {
      reportFailure(err)
      // other errors surface via store.error
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
    setForbidden(false)
    try {
      await deleteOrg(org.id)
    } catch (err) {
      reportFailure(err)
      // other errors surface via store.error
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

      {bannerText && (
        <div
          role="alert"
          className="flex items-start gap-2 p-3 rounded-lg bg-semantic-error/10 border border-semantic-error/30 text-semantic-error"
        >
          <AlertCircle size={16} className="shrink-0 mt-0.5" />
          <span className="text-sm flex-1">{bannerText}</span>
          <button onClick={dismissError} className="text-xs hover:opacity-80">
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
              {t('orgs.cancel')}
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
            // Role only, like the server (GetAndCheckAdmin): ownership grants
            // nothing by itself, so an owner whose admin role was changed saw
            // controls that could only answer 403.
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
                    <OrgChannelsSection orgId={org.id} isAdmin={canManageMembers} />
                    <OrgCustomRulesSection orgId={org.id} isAdmin={canManageMembers} />
                    {/* The org's shared Azure AI Foundry resources and models:
                        members read, admins manage; they appear alongside
                        members' own. Hidden entirely on a deployment without
                        an org keystore. Viewers and guests cannot use them
                        (the server refuses them), so they get a note. */}
                    {!orgAzureUnavailable && (
                      <div>
                        <h4
                          id={orgAzureAnchor(org.id)}
                          tabIndex={-1}
                          className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-1"
                        >
                          {t('providers.azureOrgTitle')}
                        </h4>
                        {/* Always mounted: the note is inserted into an
                            existing live region, which is what gets it
                            announced. */}
                        <p role="status" className="text-xs text-amber-400 mb-2 empty:mb-0">
                          {credentialDisabledNote?.orgId === org.id &&
                            t('orgs.sharedCredentialDisabled', {name: credentialDisabledNote.name})}
                        </p>
                        <p className="text-xs text-text-tertiary mb-3">{t('providers.azureOrgHint')}</p>
                        {canUseOrgModels(org, currentUser?.id) ? (
                          <AzureFoundryPanel
                            scope={{kind: 'org', orgId: org.id}}
                            headingLevel={5}
                            canEdit={canManageMembers}
                            anchorId={orgAzureAnchor(org.id)}
                            onConnectionSaved={handleOrgAzureConnectionSaved}
                            onUnavailable={markOrgAzureUnavailable}
                          />
                        ) : (
                          <p className="text-xs text-text-secondary">{t('providers.azureOrgViewerNote')}</p>
                        )}
                      </div>
                    )}
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
                                  onChange={e =>
                                    void handleRoleChange(
                                      m.userId,
                                      e.target.value as OrgRole,
                                      m.user?.displayName || m.user?.email || m.userId,
                                    )
                                  }
                                  disabled={m.userId === org.ownerId}
                                  className="px-2 py-1 rounded-md bg-surface-2 border border-border-default text-xs text-text-primary focus:outline-none focus:border-brand-500 disabled:opacity-50"
                                >
                                  {ROLES.map(r => (
                                    <option key={r} value={r}>
                                      {t(ROLE_LABEL_KEYS[r])}
                                    </option>
                                  ))}
                                </select>
                              ) : (
                                <span className="px-2 py-1 text-xs text-text-tertiary">{roleLabel(m.role)}</span>
                              )}
                              {canManageMembers ? (
                                <button
                                  onClick={() =>
                                    void handleRemoveMember(m.userId, m.user?.displayName || m.user?.email || m.userId)
                                  }
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
          : t('orgs.revokeFailed', {message: err instanceof Error ? err.message : t('orgs.unknownError')}),
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
