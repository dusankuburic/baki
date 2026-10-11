import {useTranslation} from 'react-i18next'
import {AlertTriangle, MoreHorizontal, Plus} from 'lucide-react'
import {Badge, Button, Dropdown, IconButton, type DropdownItem} from '@/components/shared'
import {azureBusyKey} from '@/stores/azureFoundryStore'
import type {PurposeLabelOf} from '@/lib/modelPurpose'
import {formatDateTime} from '@/lib/format'
import {absoluteTime, relativeTimeText} from '@/lib/time'
import {
  AZURE_STATUSES,
  isKnown,
  type AzureConnection,
  type AzureDiscoveryItem,
  type AzureModelEntry,
  type AzureScope,
} from '@/types'
import AzureModelList, {type ModelRowActions} from './AzureModelList'
import AzureDiscoverySuggestions, {type QuickAddResult} from './AzureDiscoverySuggestions'
import {azureHost} from './connectionForm'
import {focusKey} from './registryView'

export interface ConnectionActions {
  onAddModel: (connection: AzureConnection) => void
  onEdit: (connection: AzureConnection) => void
  onEnterCredentials: (connection: AzureConnection) => void
  onTest: (connection: AzureConnection) => void
  onRemove: (connection: AzureConnection) => void
  onQuickAdd: (connection: AzureConnection, item: AzureDiscoveryItem) => Promise<QuickAddResult>
  onAddAll: (connection: AzureConnection, items: AzureDiscoveryItem[]) => Promise<Record<string, string>>
  onAddWithDetails: (connection: AzureConnection, item: AzureDiscoveryItem) => void
  onDismissDiscovery: (connection: AzureConnection) => void
}

interface AzureConnectionGroupProps {
  scope: AzureScope
  connection: AzureConnection
  models: AzureModelEntry[]
  headingLevel: 4 | 5
  canEdit: boolean
  defaultModelId: string | null
  knowledgeEnabled: boolean
  expandDiscovery: boolean
  modelLimitReason?: string
  busy: Record<string, true>
  actions: ConnectionActions
  rowActions: ModelRowActions
  labelOf: PurposeLabelOf
}

export default function AzureConnectionGroup({
  scope,
  connection,
  models,
  headingLevel,
  canEdit,
  defaultModelId,
  knowledgeEnabled,
  expandDiscovery,
  modelLimitReason,
  busy,
  actions,
  rowActions,
  labelOf,
}: AzureConnectionGroupProps) {
  const {t} = useTranslation('settings')
  const Heading = `h${headingLevel}` as const
  const name = connection.name
  const testing = !!busy[azureBusyKey.test({connectionId: connection.id})]
  const mutating = !!busy[azureBusyKey.connection(connection.id)]
  const host = azureHost(connection.config.endpoint) || connection.config.endpoint.trim()
  const last = connection.lastTest ?? null
  const needsCredentials = connection.status === 'needs_credentials' || connection.status === 'unreadable'
  const unknownStatus = !isKnown(AZURE_STATUSES, connection.status)
  const authLabel =
    connection.config.authMode === 'entra'
      ? t('azureModels.connection.authEntra')
      : connection.config.authMode === 'entra_cli'
        ? t('azureModels.connection.authCli')
        : connection.config.authMode === 'api_key'
          ? t('azureModels.connection.authApiKey')
          : connection.config.authMode
            ? t('azureModels.connection.authOther')
            : null

  const menu: DropdownItem[] = [
    {
      type: 'item',
      label: t('azureModels.connection.test'),
      onSelect: () => actions.onTest(connection),
      disabled: testing || mutating,
    },
    {
      type: 'item',
      label: t('azureModels.connection.edit'),
      onSelect: () => actions.onEdit(connection),
      disabled: mutating,
    },
    {type: 'separator'},
    {
      type: 'item',
      label: t('azureModels.connection.remove'),
      danger: true,
      onSelect: () => actions.onRemove(connection),
      disabled: mutating,
    },
  ]

  const testLine = testing ? (
    t('azureModels.connection.testing')
  ) : last ? (
    <span title={absoluteTime(last.at)}>
      {last.ok
        ? t('azureModels.connection.testPassed', {when: relativeTimeText(last.at)})
        : t('azureModels.connection.testFailed', {when: relativeTimeText(last.at)})}
    </span>
  ) : (
    t('azureModels.connection.notTested')
  )

  const callout = needsCredentials ? (
    <Callout
      title={
        connection.status === 'unreadable'
          ? t('azureModels.status.unreadableTitle')
          : t('azureModels.status.needsCredentialsTitle')
      }
      body={
        connection.status === 'unreadable'
          ? canEdit
            ? t('azureModels.status.unreadable')
            : t('azureModels.status.unreadableMember')
          : canEdit
            ? t('azureModels.status.needsCredentials')
            : t('azureModels.status.needsCredentialsMember')
      }
      reason={connection.statusReason}
    >
      {canEdit && (
        <>
          <Button
            size="sm"
            variant="primary"
            onClick={() => actions.onEnterCredentials(connection)}
            disabled={mutating}
            aria-label={t('azureModels.connection.enterCredentialsAria', {name})}
          >
            {t('azureModels.connection.enterCredentials')}
          </Button>
          {connection.status === 'unreadable' && (
            <Button
              size="sm"
              variant="secondary"
              onClick={() => actions.onRemove(connection)}
              disabled={mutating}
              aria-label={t('azureModels.connection.removeAria', {name})}
            >
              {t('azureModels.connection.remove')}
            </Button>
          )}
        </>
      )}
    </Callout>
  ) : connection.status === 'invalid_config' ? (
    <Callout
      title={t('azureModels.status.invalidConfigTitle')}
      body={canEdit ? t('azureModels.status.invalidConfig') : t('azureModels.status.invalidConfigMember')}
      reason={connection.statusReason}
    >
      {canEdit && (
        <Button
          size="sm"
          variant="secondary"
          onClick={() => actions.onEdit(connection)}
          disabled={mutating}
          aria-label={t('azureModels.connection.editAria', {name})}
        >
          {t('azureModels.connection.edit')}
        </Button>
      )}
    </Callout>
  ) : unknownStatus ? (
    <Callout
      title={t('azureModels.status.unknownTitle')}
      body={t('azureModels.status.unknown')}
      reason={connection.statusReason}
    />
  ) : null

  const discoverable = canEdit && connection.canDiscover && connection.status === 'ready'

  return (
    <div className="rounded-lg border border-border-default bg-surface-1 p-4" aria-busy={mutating || undefined}>
      <div className="flex flex-wrap items-start gap-2">
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <Heading
              tabIndex={-1}
              data-focus={focusKey.connection(connection.id)}
              className="min-w-0 break-all text-sm font-semibold text-text-primary"
            >
              {name}
            </Heading>
            {authLabel && <Badge tone="neutral">{authLabel}</Badge>}
          </div>
          <p className="mt-0.5 break-all text-xs text-text-tertiary">
            {host && (
              <>
                {host}
                {' · '}
              </>
            )}
            {t('azureModels.connection.modelCount', {count: models.length})}
            {' · '}
            {testLine}
          </p>
          {last && !last.ok && last.error && !testing && (
            <p className="mt-0.5 wrap-break-word text-2xs text-text-tertiary">{last.error}</p>
          )}
          {scope.kind === 'org' && (
            <p className="mt-0.5 text-xs text-text-tertiary">
              {connection.sharedBy
                ? t('azureModels.connection.sharedBy', {name: connection.sharedBy})
                : t('azureModels.connection.sharedByOrg')}
              {connection.updatedAt && (
                <>
                  {' · '}
                  <time dateTime={connection.updatedAt}>
                    {t('azureModels.connection.updated', {date: formatDateTime(connection.updatedAt)})}
                  </time>
                </>
              )}
            </p>
          )}
        </div>
        {canEdit && (
          <div className="flex shrink-0 items-center gap-1">
            <Button
              size="sm"
              variant="secondary"
              icon={Plus}
              onClick={() => actions.onAddModel(connection)}
              disabled={!!modelLimitReason}
              title={modelLimitReason}
              aria-label={t('azureModels.connection.addModelAria', {name})}
              data-focus={focusKey.addModel(connection.id)}
            >
              {t('azureModels.section.addModel')}
            </Button>
            <Dropdown
              align="end"
              items={menu}
              trigger={
                <IconButton icon={MoreHorizontal} size="sm" label={t('azureModels.connection.moreActions', {name})} />
              }
            />
          </div>
        )}
      </div>

      {callout}

      {models.length > 0 ? (
        <div className="mt-3">
          <AzureModelList
            label={t('azureModels.models.listLabel', {name})}
            models={models}
            readOnly={!canEdit}
            row={entry => ({
              isDefault: defaultModelId === entry.modelId,
              knowledgeEnabled,
              actions: canEdit ? rowActions : undefined,
              testing: !!busy[azureBusyKey.test({model: {id: entry.id}})],
              busy: !!busy[azureBusyKey.model(entry.id)],
              labelOf,
            })}
          />
        </div>
      ) : !canEdit ? (
        <p className="mt-3 text-xs text-text-tertiary">{t('azureModels.connection.noModelsReadOnly')}</p>
      ) : connection.config.authMode === 'api_key' ? (
        <p className="mt-3 text-xs text-text-tertiary">{t('azureModels.connection.noModelsApiKey')}</p>
      ) : !discoverable ? (
        <p className="mt-3 text-xs text-text-tertiary">{t('azureModels.connection.noModels')}</p>
      ) : null}

      {discoverable && (
        <AzureDiscoverySuggestions
          scope={scope}
          connection={connection}
          defaultExpanded={expandDiscovery || models.length === 0}
          addBlockedReason={modelLimitReason}
          onQuickAdd={item => actions.onQuickAdd(connection, item)}
          onAddAll={items => actions.onAddAll(connection, items)}
          onAddWithDetails={item => actions.onAddWithDetails(connection, item)}
          onDismiss={() => actions.onDismissDiscovery(connection)}
        />
      )}
    </div>
  )
}

function Callout({
  title,
  body,
  reason,
  children,
}: {
  title: string
  body: string
  reason?: string
  children?: React.ReactNode
}) {
  return (
    <div className="mt-3 flex items-start gap-2 rounded-lg border border-semantic-warning/30 bg-semantic-warning/10 px-3 py-2">
      <AlertTriangle size={14} aria-hidden="true" className="mt-0.5 shrink-0 text-semantic-warning" />
      <div className="min-w-0 flex-1 space-y-1 text-xs text-text-secondary">
        <p className="text-sm font-medium text-text-primary">{title}</p>
        <p>{body}</p>
        {reason && <p className="wrap-break-word">{reason}</p>}
        {children && <div className="flex flex-wrap gap-2 pt-1">{children}</div>}
      </div>
    </div>
  )
}
