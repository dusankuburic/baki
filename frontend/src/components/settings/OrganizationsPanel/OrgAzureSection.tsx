import {useTranslation} from 'react-i18next'
import type {AuthUser} from '@/api/auth'
import type {Organisation} from '@/stores/orgStore'
import AzureFoundryPanel from '../azure/AzureFoundryPanel'
import {orgAzureAnchor} from '../azure/registryView'
import {canUseOrgModels} from './helpers'

interface OrgAzureSectionProps {
  org: Organisation
  currentUser: AuthUser | null
  canManageMembers: boolean
  credentialDisabledNote: {orgId: string; name: string} | null
  handleOrgAzureConnectionSaved: () => void
  markOrgAzureUnavailable: () => void
}

export default function OrgAzureSection({
  org,
  currentUser,
  canManageMembers,
  credentialDisabledNote,
  handleOrgAzureConnectionSaved,
  markOrgAzureUnavailable,
}: OrgAzureSectionProps) {
  const {t} = useTranslation('settings')
  return (
    <div>
      <h4
        id={orgAzureAnchor(org.id)}
        tabIndex={-1}
        className="text-xs font-bold uppercase tracking-wider text-text-tertiary mb-1"
      >
        {t('providers.azureOrgTitle')}
      </h4>
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
  )
}
