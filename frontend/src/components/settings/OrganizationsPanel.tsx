import {useCallback, useEffect, useState, useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {AlertCircle, Building2} from 'lucide-react'
import {useOrgStore, type Organisation, type OrgRole} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {useChatStore} from '@/stores/chatStore'
import {useAzureFoundryStore} from '@/stores/azureFoundryStore'
import {useUIStore} from '@/stores/uiStore'
import {useConfirm} from '@/components/shared'
import {orgIdFromAnchor} from './azure/registryView'
import {ApiError} from '@/api/client'
import {useToast} from '@/components/shared/Toast'
import {emailErrorKey} from '@/lib/validators'
import {EMAIL_MAX_LENGTH} from '@/lib/constraints'
import {canUseOrgModels, inviteLinkURL, orgNameId} from './OrganizationsPanel/helpers'
import OrgCreateSection from './OrganizationsPanel/OrgCreateSection'
import OrgListSection from './OrganizationsPanel/OrgListSection'

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
  const bumpProviderEpoch = useChatStore(s => s.bumpProviderEpoch)
  const {confirm} = useConfirm()

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

  const [orgAzureUnavailable, setOrgAzureUnavailable] = useState(false)
  const markOrgAzureUnavailable = useCallback(() => setOrgAzureUnavailable(true), [])
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
  const handleOrgAzureConnectionSaved = useCallback(() => setCredentialDisabledNote(null), [])

  const settingsAnchor = useUIStore(s => s.settingsAnchor)
  useEffect(() => {
    const anchoredOrg = orgIdFromAnchor(settingsAnchor)
    if (!anchoredOrg || isLoading) return
    const org = organisations.find(o => o.id === anchoredOrg)
    if (org && org.id === activeOrgId && !orgAzureUnavailable && canUseOrgModels(org, currentUser?.id)) return
    if (org) document.getElementById(orgNameId(org.id))?.focus()
    useUIStore.getState().clearSettingsAnchor()
  }, [settingsAnchor, isLoading, organisations, activeOrgId, orgAzureUnavailable, currentUser?.id])

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

  const handleInvite = async () => {
    const email = inviteEmail.trim()
    if (!selected || !email) return
    const emailKey = emailErrorKey(email)
    if (emailKey) {
      setInviteError(t(emailKey, {ns: 'common', max: EMAIL_MAX_LENGTH}))
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
    }
  }

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
    }
  }

  const banner = bannerText ? (
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
  ) : null

  return (
    <div className="space-y-6">
      <OrgCreateSection
        banner={banner}
        createOrg={createOrg}
        setActiveOrg={setActiveOrg}
        setForbidden={setForbidden}
        reportFailure={reportFailure}
      />

      {isLoading && organisations.length === 0 ? (
        <div className="py-8 text-center text-sm text-text-tertiary">{t('orgs.loading')}</div>
      ) : organisations.length === 0 ? (
        <div className="py-8 text-center">
          <Building2 size={32} className="mx-auto text-text-tertiary/50" />
          <p className="mt-2 text-sm text-text-tertiary">{t('orgs.empty')}</p>
        </div>
      ) : (
        <OrgListSection
          organisations={organisations}
          activeOrgId={activeOrgId}
          selected={selected}
          currentUser={currentUser}
          setActiveOrg={setActiveOrg}
          setForbidden={setForbidden}
          reportFailure={reportFailure}
          renameOrg={renameOrg}
          handleDeleteOrg={handleDeleteOrg}
          handleLeave={handleLeave}
          handleRemoveMember={handleRemoveMember}
          handleRoleChange={handleRoleChange}
          orgAzureUnavailable={orgAzureUnavailable}
          credentialDisabledNote={credentialDisabledNote}
          markOrgAzureUnavailable={markOrgAzureUnavailable}
          handleOrgAzureConnectionSaved={handleOrgAzureConnectionSaved}
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
  )
}
