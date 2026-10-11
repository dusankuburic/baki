import {useId} from 'react'
import {useTranslation} from 'react-i18next'
import {Button} from '@/components/shared'
import {useAuthStore} from '@/stores/authStore'
import {useOrgStore} from '@/stores/orgStore'
import {useUIStore} from '@/stores/uiStore'
import type {PurposeLabelOf} from '@/lib/modelPurpose'
import AzureModelList from './AzureModelList'
import {orgAzureAnchor, type SharedOrgGroup} from './registryView'

interface AzureSharedModelsProps {
  groups: SharedOrgGroup[]
  headingLevel: 4 | 5
  defaultModelId: string | null
  knowledgeEnabled: boolean
  labelOf: PurposeLabelOf
}

export default function AzureSharedModels({
  groups,
  headingLevel,
  defaultModelId,
  knowledgeEnabled,
  labelOf,
}: AzureSharedModelsProps) {
  if (groups.length === 0) return null
  return (
    <div className="space-y-4">
      {groups.map(group => (
        <SharedGroup
          key={group.orgId}
          group={group}
          headingLevel={headingLevel}
          defaultModelId={defaultModelId}
          knowledgeEnabled={knowledgeEnabled}
          labelOf={labelOf}
        />
      ))}
    </div>
  )
}

function SharedGroup({
  group,
  headingLevel,
  defaultModelId,
  knowledgeEnabled,
  labelOf,
}: {
  group: SharedOrgGroup
  headingLevel: 4 | 5
  defaultModelId: string | null
  knowledgeEnabled: boolean
  labelOf: PurposeLabelOf
}) {
  const {t} = useTranslation('settings')
  const headingId = useId()
  const userId = useAuthStore(s => s.user?.id)
  const isAdmin = useOrgStore(
    s => s.organisations.find(o => o.id === group.orgId)?.members?.find(m => m.userId === userId)?.role === 'admin',
  )
  const Heading = `h${headingLevel}` as const
  return (
    <div className="rounded-lg border border-border-default bg-surface-1 p-4">
      <div className="flex flex-wrap items-start gap-2">
        <div className="min-w-0 flex-1">
          <Heading id={headingId} className="wrap-break-word text-sm font-semibold text-text-primary">
            {t('azureModels.shared.heading', {org: group.orgName})}
          </Heading>
          <p className="mt-0.5 text-xs text-text-tertiary">{t('azureModels.shared.readOnly')}</p>
        </div>
        {isAdmin && (
          <Button
            size="sm"
            variant="secondary"
            aria-describedby={headingId}
            onClick={() => useUIStore.getState().openSettings('orgs', orgAzureAnchor(group.orgId))}
          >
            {t('azureModels.shared.manage')}
          </Button>
        )}
      </div>
      <div className="mt-3">
        <AzureModelList
          label={t('azureModels.shared.heading', {org: group.orgName})}
          models={group.models}
          readOnly
          row={entry => ({
            isDefault: defaultModelId === entry.modelId,
            knowledgeEnabled,
            sharedOrgName: group.orgName,
            showResource: true,
            labelOf,
          })}
        />
      </div>
    </div>
  )
}
