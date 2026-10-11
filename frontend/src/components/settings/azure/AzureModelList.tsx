import {useTranslation} from 'react-i18next'
import {CheckCircle2, Circle, Loader2, MoreHorizontal, Users, XCircle} from 'lucide-react'
import clsx from 'clsx'
import {Badge, Button, Dropdown, IconButton, type DropdownItem} from '@/components/shared'
import {modelKindLabelKey, purposeLabelKey, resolvePurpose, type PurposeLabelOf} from '@/lib/modelPurpose'
import {formatContextWindow} from '@/lib/format'
import {absoluteTime, relativeTimeText} from '@/lib/time'
import {AZURE_MODEL_KINDS, AZURE_STATUSES, isKnown, type AzureModelEntry, type AzureStatus} from '@/types'
import {focusKey, isChatKind} from './registryView'

export interface ModelRowActions {
  onEdit?: (entry: AzureModelEntry) => void
  onTest?: (entry: AzureModelEntry) => void
  onMakeDefault?: (entry: AzureModelEntry) => void
  onMakeOrgDefault?: (entry: AzureModelEntry) => void
  onUseForKnowledge?: (entry: AzureModelEntry) => void
  onRemove?: (entry: AzureModelEntry) => void
}

export interface AzureModelRowProps {
  entry: AzureModelEntry
  isDefault?: boolean
  knowledgeEnabled?: boolean
  actions?: ModelRowActions
  testing?: boolean
  busy?: boolean
  sharedOrgName?: string
  showResource?: boolean
  labelOf?: PurposeLabelOf
}

const TEST_UNAVAILABLE: Partial<Record<AzureStatus, 'needsCredentials' | 'invalidConfig' | 'unsupported'>> = {
  needs_credentials: 'needsCredentials',
  unreadable: 'needsCredentials',
  invalid_config: 'invalidConfig',
  unsupported: 'unsupported',
}

export function AzureModelRow({
  entry,
  isDefault = false,
  knowledgeEnabled = false,
  actions,
  testing = false,
  busy = false,
  sharedOrgName,
  showResource = false,
  labelOf,
}: AzureModelRowProps) {
  const {t} = useTranslation('settings')
  const name = entry.displayName || entry.deployment
  const purpose = resolvePurpose(entry, labelOf)
  const purposeText = purpose.kind === 'preset' ? t(purposeLabelKey(purpose.preset), {ns: 'common'}) : purpose.text
  const kindText = t(modelKindLabelKey(entry.kind), {ns: 'common'})
  const knownKind = isKnown(AZURE_MODEL_KINDS, entry.kind)
  const knownStatus = isKnown(AZURE_STATUSES, entry.status)
  const contextWindow = entry.kind === 'embedding' ? undefined : (entry.contextWindow ?? entry.reportedContextWindow)
  const last = entry.lastTest ?? null
  const unavailable = isKnown(AZURE_STATUSES, entry.status) ? TEST_UNAVAILABLE[entry.status] : undefined
  const statusNote = !knownStatus
    ? entry.statusReason || t('azureModels.status.rowUnknown')
    : entry.status === 'needs_credentials' || entry.status === 'unreadable'
      ? t('azureModels.status.rowNeedsCredentials')
      : entry.status === 'invalid_config'
        ? t('azureModels.status.rowInvalidConfig')
        : entry.status === 'unsupported'
          ? entry.statusReason || t('azureModels.status.rowUnsupported')
          : null

  const items: DropdownItem[] = []
  if (actions?.onTest) {
    const onTest = actions.onTest
    items.push({
      type: 'item',
      label: t('azureModels.model.test'),
      onSelect: () => onTest(entry),
      disabled: testing || busy,
      disabledReason: unavailable
        ? t(
            unavailable === 'needsCredentials'
              ? 'azureModels.model.testNeedsCredentials'
              : unavailable === 'invalidConfig'
                ? 'azureModels.model.testInvalidConfig'
                : 'azureModels.model.testUnsupported',
          )
        : undefined,
    })
  }
  if (actions?.onMakeDefault && isChatKind(entry.kind) && !isDefault) {
    const onMakeDefault = actions.onMakeDefault
    items.push({
      type: 'item',
      label: t('azureModels.model.makeDefault'),
      onSelect: () => onMakeDefault(entry),
      disabled: busy,
    })
  }
  if (actions?.onMakeOrgDefault && isChatKind(entry.kind) && !entry.isDefault) {
    const onMakeOrgDefault = actions.onMakeOrgDefault
    items.push({
      type: 'item',
      label: t('azureModels.model.makeOrgDefault'),
      onSelect: () => onMakeOrgDefault(entry),
      disabled: busy,
    })
  }
  if (actions?.onUseForKnowledge && knowledgeEnabled && entry.kind === 'embedding' && !entry.isKnowledgeEmbedding) {
    const onUseForKnowledge = actions.onUseForKnowledge
    items.push({
      type: 'item',
      label: t('azureModels.model.useForKnowledge'),
      onSelect: () => onUseForKnowledge(entry),
      disabled: busy,
    })
  }
  if (actions?.onRemove) {
    const onRemove = actions.onRemove
    if (items.length > 0) items.push({type: 'separator'})
    items.push({
      type: 'item',
      label: t('azureModels.model.remove'),
      danger: true,
      onSelect: () => onRemove(entry),
      disabled: busy,
    })
  }
  const hasActions = !!actions?.onEdit || items.length > 0

  return (
    <li className="azure-model-row py-2.5" aria-busy={busy || testing || undefined}>
      <div className="azure-model-main">
        <div className="flex min-w-0 flex-wrap items-center gap-1.5">
          <span
            tabIndex={-1}
            data-focus={focusKey.modelName(entry.id)}
            className="min-w-0 truncate text-sm font-medium text-text-primary"
            title={name}
          >
            {name}
          </span>
          {entry.kind !== 'chat' && (
            <Badge tone="info" srLabel={t('azureModels.model.badgeKindSr', {kind: kindText})}>
              {kindText}
            </Badge>
          )}
          {isDefault && (
            <Badge
              tone="brand"
              srLabel={t('azureModels.model.badgeDefaultSr')}
              title={t('azureModels.model.badgeDefaultSr')}
            >
              {t('azureModels.model.badgeDefault')}
            </Badge>
          )}
          {entry.isDefault && (
            <Badge
              tone="brand"
              srLabel={t('azureModels.model.badgeOrgDefaultHint')}
              title={t('azureModels.model.badgeOrgDefaultHint')}
            >
              {t('azureModels.model.badgeOrgDefault')}
            </Badge>
          )}
          {knowledgeEnabled && entry.isKnowledgeEmbedding && (
            <Badge
              tone="success"
              srLabel={t('azureModels.model.badgeKnowledgeSr')}
              title={t('azureModels.model.badgeKnowledgeSr')}
            >
              {t('azureModels.model.badgeKnowledge')}
            </Badge>
          )}
          {sharedOrgName && (
            <Badge
              tone="neutral"
              icon={Users}
              srLabel={t('azureModels.model.badgeSharedSr', {org: sharedOrgName})}
              title={t('azureModels.model.badgeSharedSr', {org: sharedOrgName})}
            >
              {t('azureModels.model.badgeShared')}
            </Badge>
          )}
        </div>
        <p className="mt-0.5 wrap-break-word text-xs text-text-tertiary">
          <span className="font-mono">{entry.deployment}</span>
          {showResource && (
            <>
              {' · '}
              {t('azureModels.model.resourceLine', {name: entry.connectionName})}
            </>
          )}
          {entry.description && (
            <>
              {' · '}
              {entry.description}
            </>
          )}
        </p>
        {statusNote && <p className="mt-0.5 text-xs text-text-secondary">{statusNote}</p>}
        {last && !last.ok && last.error && (
          <p className="mt-0.5 line-clamp-2 wrap-break-word text-2xs text-text-tertiary">{last.error}</p>
        )}
      </div>

      <div className="azure-model-facts text-xs text-text-secondary">
        <span className="azure-model-purpose min-w-0 wrap-break-word">
          <span className="azure-cell-label">{t('azureModels.models.cellPurpose')}</span> {purposeText}
        </span>
        <span className="azure-model-context">
          <span className="azure-cell-label">{t('azureModels.models.cellContext')}</span>{' '}
          {contextWindow ? (
            formatContextWindow(contextWindow)
          ) : (
            <>
              <span aria-hidden="true">—</span>
              <span className="sr-only">
                {entry.kind === 'embedding'
                  ? t('azureModels.models.contextNone')
                  : t('azureModels.models.contextUnknown')}
              </span>
            </>
          )}
        </span>
      </div>

      <div className="azure-model-test text-xs text-text-secondary">
        <span className="azure-cell-label">{t('azureModels.models.cellTest')}</span>{' '}
        {testing ? (
          <span className="inline-flex items-center gap-1">
            <Loader2 size={12} aria-hidden="true" className="shrink-0 animate-spin" />
            {t('azureModels.models.testing')}
          </span>
        ) : last ? (
          <span className="inline-flex flex-wrap items-center gap-x-1" title={absoluteTime(last.at)}>
            {last.ok ? (
              <CheckCircle2 size={12} aria-hidden="true" className="shrink-0 text-semantic-success" />
            ) : (
              <XCircle size={12} aria-hidden="true" className="shrink-0 text-semantic-error" />
            )}
            <span>{last.ok ? t('azureModels.models.passed') : t('azureModels.models.failed')}</span>
            <span className="azure-test-when">
              {'· '}
              {relativeTimeText(last.at)}
            </span>
          </span>
        ) : (
          <span className="inline-flex items-center gap-1">
            <Circle size={12} aria-hidden="true" className="shrink-0 text-text-tertiary" />
            {t('azureModels.models.notTested')}
          </span>
        )}
      </div>

      {hasActions && (
        <div className="azure-model-actions">
          {actions?.onEdit && (
            <Button
              size="sm"
              variant="ghost"
              onClick={() => actions.onEdit?.(entry)}
              aria-label={t('azureModels.model.editAria', {name})}
              data-focus={focusKey.modelEdit(entry.id)}
              disabled={busy || !knownKind}
              title={knownKind ? undefined : t('azureModels.model.editUnknownKind')}
            >
              {t('azureModels.model.edit')}
            </Button>
          )}
          {items.length > 0 && (
            <Dropdown
              align="end"
              items={items}
              trigger={
                <IconButton icon={MoreHorizontal} size="sm" label={t('azureModels.model.moreActions', {name})} />
              }
            />
          )}
        </div>
      )}
    </li>
  )
}

interface AzureModelListProps {
  label: string
  models: AzureModelEntry[]
  row: (entry: AzureModelEntry) => Omit<AzureModelRowProps, 'entry'>
  readOnly?: boolean
}

export default function AzureModelList({label, models, row, readOnly = false}: AzureModelListProps) {
  const {t} = useTranslation('settings')
  return (
    <div className={clsx('azure-models', readOnly && 'azure-models--readonly')}>
      <div
        aria-hidden="true"
        className="azure-models-head border-b border-border-subtle pb-1 text-2xs font-semibold uppercase tracking-wider text-text-tertiary"
      >
        <span className="azure-col-model">{t('azureModels.models.colModel')}</span>
        <span className="azure-col-purpose">{t('azureModels.models.colPurpose')}</span>
        <span className="azure-col-context">{t('azureModels.models.colContext')}</span>
        <span className="azure-col-test">{t('azureModels.models.colTest')}</span>
      </div>
      <ul aria-label={label} className="divide-y divide-border-subtle">
        {models.map(m => (
          <AzureModelRow key={`${m.orgId ?? ''}/${m.id}`} entry={m} {...row(m)} />
        ))}
      </ul>
    </div>
  )
}
