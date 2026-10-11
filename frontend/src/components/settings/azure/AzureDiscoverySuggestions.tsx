import {useEffect, useId, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Loader2} from 'lucide-react'
import {Button} from '@/components/shared'
import {
  EMPTY_AZURE_SCOPE,
  azureBusyKey,
  azureScopeKey,
  selectAzureScope,
  useAzureFoundryStore,
} from '@/stores/azureFoundryStore'
import {formatContextWindow} from '@/lib/format'
import {modelKindLabelKey} from '@/lib/modelPurpose'
import type {AzureConnection, AzureDiscoveryItem, AzureScope} from '@/types'
import {discoveryItemKey, focusKey, unaddedCounts, unaddedItems} from './registryView'

export type QuickAddResult = {ok: true} | {ok: false; error: string}

interface AzureDiscoverySuggestionsProps {
  scope: AzureScope
  connection: AzureConnection
  defaultExpanded: boolean
  addBlockedReason?: string
  onQuickAdd: (item: AzureDiscoveryItem) => Promise<QuickAddResult>
  onAddAll: (items: AzureDiscoveryItem[]) => Promise<Record<string, string>>
  onAddWithDetails: (item: AzureDiscoveryItem) => void
  onDismiss: () => void
}

type RowState = {phase: 'adding'} | {phase: 'failed'; error: string}

export default function AzureDiscoverySuggestions({
  scope,
  connection,
  defaultExpanded,
  addBlockedReason,
  onQuickAdd,
  onAddAll,
  onAddWithDetails,
  onDismiss,
}: AzureDiscoverySuggestionsProps) {
  const {t} = useTranslation('settings')
  const discover = useAzureFoundryStore(s => s.discover)
  const state = useAzureFoundryStore(s => selectAzureScope(scope)(s).discovery[connection.id])
  const busyDiscovering = useAzureFoundryStore(
    s => !!(s.scopes[azureScopeKey(scope)] ?? EMPTY_AZURE_SCOPE).busy[azureBusyKey.discover(connection.id)],
  )
  const [expanded, setExpanded] = useState(defaultExpanded)
  const [rows, setRows] = useState<Record<string, RowState>>({})
  const [addingAll, setAddingAll] = useState(false)
  const rootRef = useRef<HTMLDivElement>(null)
  const [pendingFocus, setPendingFocus] = useState<string | null>(null)

  const uid = useId()
  const listId = `${uid}-list`
  const summaryId = `${uid}-summary`

  const run = (force: boolean) => {
    discover(scope, connection.id, {force}).catch(() => {
    })
  }

  const auto = connection.modelCount === 0 || connection.suggestDiscovery || defaultExpanded
  useEffect(() => {
    if (auto) discover(scope, connection.id).catch(() => {})
  }, [auto, connection.id, scope, discover])

  useEffect(() => {
    if (!pendingFocus) return
    const target = [...(rootRef.current?.querySelectorAll<HTMLElement>('[data-focus]') ?? [])].find(
      el => el.dataset.focus === pendingFocus,
    )
    target?.focus()
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot: cleared once used
    setPendingFocus(null)
  }, [pendingFocus])

  const data = state?.data ?? null
  const loading = (state?.status === 'loading' || busyDiscovering) && !data
  const failure = state?.status === 'error' ? state.error : (data?.error ?? null)
  const items = data ? [...data.deployments, ...data.agents] : []
  const unadded = unaddedItems(data)
  const counts = unaddedCounts(data)
  const canAdd = !addBlockedReason
  const review = connection.suggestDiscovery && unadded.length > 0

  const toggle = () => {
    const next = !expanded
    setExpanded(next)
    if (next && !data && state?.status !== 'loading') run(false)
  }

  const summary = (() => {
    if (!data || data.error) {
      return loading ? t('azureModels.discovery.loading') : t('azureModels.discovery.find')
    }
    if (connection.modelCount === 0) return t('azureModels.discovery.headingCount', {count: unadded.length})
    if (unadded.length === 0) return t('azureModels.discovery.allAdded')
    if (connection.suggestDiscovery) return t('azureModels.discovery.unadded', {count: unadded.length})
    const deployments =
      counts.deployments > 0 ? t('azureModels.discovery.deployments', {count: counts.deployments}) : ''
    const agents = counts.agents > 0 ? t('azureModels.discovery.agents', {count: counts.agents}) : ''
    return t('azureModels.discovery.summary', {
      summary: deployments && agents ? t('azureModels.discovery.joiner', {deployments, agents}) : deployments || agents,
    })
  })()

  const focusAfterAdd = (key: string): string => {
    const order = unadded.map(discoveryItemKey)
    const at = order.indexOf(key)
    const next = order[at + 1] ?? (at > 0 ? order[at - 1] : undefined)
    return next ? focusKey.discoveryAdd(connection.id, next) : focusKey.discoveryToggle(connection.id)
  }

  const quickAdd = async (item: AzureDiscoveryItem) => {
    const key = discoveryItemKey(item)
    setRows(prev => ({...prev, [key]: {phase: 'adding'}}))
    const result = await onQuickAdd(item)
    setRows(prev => {
      const out = {...prev}
      if (result.ok) delete out[key]
      else out[key] = {phase: 'failed', error: result.error}
      return out
    })
    setPendingFocus(result.ok ? focusAfterAdd(key) : focusKey.discoveryAdd(connection.id, key))
  }

  const addAll = async () => {
    const todo = unaddedItems(data)
    if (todo.length === 0) return
    setAddingAll(true)
    setRows(Object.fromEntries(todo.map(item => [discoveryItemKey(item), {phase: 'adding'} as const])))
    const failures = await onAddAll(todo)
    setRows(Object.fromEntries(Object.entries(failures).map(([k, error]) => [k, {phase: 'failed', error} as const])))
    setAddingAll(false)
    setPendingFocus(focusKey.discoveryToggle(connection.id))
  }

  return (
    <div ref={rootRef} className="mt-3 border-t border-border-subtle pt-3">
      <div className="flex flex-wrap items-center gap-2">
        <p id={summaryId} className="min-w-0 flex-1 text-sm text-text-secondary">
          {loading && <Loader2 size={12} aria-hidden="true" className="mr-1 inline animate-spin" />}
          {summary}
        </p>
        {canAdd && unadded.length > 0 && (
          <Button
            size="sm"
            variant="secondary"
            onClick={() => void addAll()}
            loading={addingAll}
            aria-label={
              addingAll
                ? t('azureModels.discovery.addingAllAria', {name: connection.name})
                : t('azureModels.discovery.addAllAria', {name: connection.name})
            }
          >
            {addingAll ? t('azureModels.discovery.adding') : t('azureModels.discovery.addAll')}
          </Button>
        )}
        <Button
          size="sm"
          variant="ghost"
          onClick={toggle}
          aria-expanded={expanded}
          aria-controls={listId}
          aria-describedby={summaryId}
          aria-label={
            expanded
              ? t('azureModels.discovery.hideAria', {name: connection.name})
              : review
                ? t('azureModels.discovery.reviewAria', {name: connection.name})
                : t('azureModels.discovery.showAria', {name: connection.name})
          }
          data-focus={focusKey.discoveryToggle(connection.id)}
        >
          {expanded
            ? t('azureModels.discovery.hide')
            : review
              ? t('azureModels.discovery.review')
              : t('azureModels.discovery.show')}
        </Button>
        {connection.suggestDiscovery && (
          <Button
            size="sm"
            variant="ghost"
            onClick={onDismiss}
            aria-label={t('azureModels.discovery.dismissAria', {name: connection.name})}
          >
            {t('azureModels.discovery.dismiss')}
          </Button>
        )}
      </div>
      {addBlockedReason && unadded.length > 0 && <p className="mt-1 text-xs text-text-tertiary">{addBlockedReason}</p>}

      <div id={listId} hidden={!expanded} className="mt-2 space-y-2">
        {expanded && failure && (
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <p className="text-text-primary">{t('azureModels.discovery.failed', {reason: failure})}</p>
            <Button
              size="sm"
              variant="secondary"
              onClick={() => run(true)}
              disabled={busyDiscovering}
              aria-label={t('azureModels.discovery.retryAria', {name: connection.name})}
            >
              {t('azureModels.discovery.retry')}
            </Button>
            <p className="w-full text-text-tertiary">{t('azureModels.discovery.failedHint')}</p>
          </div>
        )}
        {expanded && data && !data.error && (
          <>
            {data.warnings.map(w => (
              <p key={w} className="text-xs text-text-tertiary">
                {w}
              </p>
            ))}
            {items.length === 0 ? (
              <p className="text-xs text-text-tertiary">{t('azureModels.discovery.empty')}</p>
            ) : (
              <ul
                aria-label={t('azureModels.discovery.listLabel', {name: connection.name})}
                className="divide-y divide-border-subtle rounded-md border border-border-subtle"
              >
                {items.map((item, index) => {
                  const key = discoveryItemKey(item)
                  const row = rows[key]
                  const kind = t(modelKindLabelKey(item.kind), {ns: 'common'})
                  const errorId = `${uid}-item-${index}-error`
                  return (
                    <li key={key} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2">
                      <div className="min-w-0 flex-1 text-xs">
                        <span className="break-all font-mono text-sm text-text-primary">{item.name}</span>
                        <span className="text-text-tertiary">
                          {' · '}
                          {kind}
                          {item.model && item.model !== item.name && (
                            <>
                              {' · '}
                              {item.model}
                            </>
                          )}
                          {item.contextWindow ? (
                            <>
                              {' · '}
                              {formatContextWindow(item.contextWindow)}
                            </>
                          ) : null}
                          {item.workflow && (
                            <>
                              {' · '}
                              {t('azureModels.discovery.workflow')}
                            </>
                          )}
                        </span>
                        {row?.phase === 'failed' && (
                          <p id={errorId} className="mt-0.5 text-semantic-error">
                            {t('azureModels.discovery.addFailed', {reason: row.error})}
                          </p>
                        )}
                      </div>
                      {item.alreadyAdded ? (
                        <span className="text-xs font-medium text-text-secondary">
                          {t('azureModels.discovery.added')}
                        </span>
                      ) : (
                        <div className="flex items-center gap-1">
                          <Button
                            size="sm"
                            variant="secondary"
                            onClick={() => void quickAdd(item)}
                            loading={row?.phase === 'adding'}
                            disabled={!canAdd || addingAll}
                            aria-label={t('azureModels.discovery.addAria', {name: item.name, kind})}
                            aria-describedby={row?.phase === 'failed' ? errorId : undefined}
                            data-focus={focusKey.discoveryAdd(connection.id, key)}
                          >
                            {t('azureModels.discovery.add')}
                          </Button>
                          <Button
                            size="sm"
                            variant="ghost"
                            onClick={() => onAddWithDetails(item)}
                            disabled={!canAdd || addingAll}
                            aria-label={t('azureModels.discovery.addWithDetailsAria', {name: item.name})}
                          >
                            {t('azureModels.discovery.addWithDetails')}
                          </Button>
                        </div>
                      )}
                    </li>
                  )
                })}
              </ul>
            )}
            <Button
              size="sm"
              variant="ghost"
              onClick={() => run(true)}
              disabled={busyDiscovering}
              aria-label={t('azureModels.discovery.refreshAria', {name: connection.name})}
            >
              {t('azureModels.discovery.refresh')}
            </Button>
          </>
        )}
      </div>
    </div>
  )
}
