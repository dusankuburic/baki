import {getErrorCode} from '@/lib/getErrorMessage'
import {useCallback, useEffect, useId, useMemo, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {Cloud, Info, Plus, X} from 'lucide-react'
import clsx from 'clsx'
import {Button, EmptyState, IconButton, Skeleton, useConfirm} from '@/components/shared'
import {ApiError} from '@/api/client'
import {selectAzureScope, useAzureFoundryStore} from '@/stores/azureFoundryStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {useSystemStore} from '@/stores/systemStore'
import {useUIStore} from '@/stores/uiStore'
import {isTauri} from '@/platform/guards'
import {purposeLabelKey} from '@/lib/modelPurpose'
import {formatCount} from '@/lib/format'
import type {AzureConnection, AzureDiscoveryItem, AzureModelEntry, AzureScope, ModelPurposePreset} from '@/types'
import AzureConnectionGroup, {type ConnectionActions} from './AzureConnectionGroup'
import AzureConnectionModal, {type ConnectionModalMode} from './AzureConnectionModal'
import AzureModelModal from './AzureModelModal'
import AzureSharedModels from './AzureSharedModels'
import type {ModelRowActions} from './AzureModelList'
import type {QuickAddResult} from './AzureDiscoverySuggestions'
import {
  discoveryQuickAddInput,
  emptyModelForm,
  modelFormFromDiscovery,
  modelFormFromEntry,
  type ModelFormState,
} from './modelForm'
import {
  discoveryItemKey,
  effectiveDefaultModelId,
  focusAfterConnectionRemoval,
  focusAfterModelRemoval,
  focusKey,
  isChatKind,
  isOwnItem,
  limitsView,
  sharedGroups,
} from './registryView'

export interface AzureFoundryPanelProps {
  scope: AzureScope
  /** The resource cards' heading level: 4 under AI Accounts' h3, 5 in an organisation's block. */
  headingLevel: 4 | 5
  /** Org scope: the deployment has no org keystore. The panel renders nothing; the host hides its heading. */
  onUnavailable?: () => void
  /** Org scope: the caller administers the organisation. Its members get a read-only view. */
  canEdit?: boolean
  /** The host's section heading (tabIndex -1): a Settings deep link naming it is focused once the data loaded. */
  anchorId?: string
  /** A resource was added, edited or given its credentials again. */
  onConnectionSaved?: (connection: AzureConnection) => void
  /** The scope's first chat model (chat, Claude or agent) was added. */
  onFirstChatModel?: () => void
}

type ConnectionModalState = {mode: ConnectionModalMode; connection: AzureConnection | null}
type ModelModalState = {entry: AzureModelEntry | null; initial: ModelFormState; reportedWindow?: number}

function isCode(err: unknown, code: string): boolean {
  return getErrorCode(err) === code
}

// AI Accounts → Azure AI Foundry, and an organisation's block in
// Organizations: the model registry of one scope. It owns
//   - the states: first run, resources without models, the migrated
//     resource and its discovery prompt, discovery suggestions,
//     credentials needed, the organisation's view, loading and failures, a
//     desktop list file it cannot write or had to recover, invalid
//     settings and unsupported models;
//   - the Resource and Model modals;
//   - one sr-only status region and one alert region, both mounted up front
//     (no toasts: they would render outside the Settings dialog);
//   - where focus goes after an action (a new resource's heading, a new
//     model's name, the next row after a removal, the anchored heading of a
//     deep link).
export default function AzureFoundryPanel({
  scope: scopeProp,
  headingLevel,
  onUnavailable,
  canEdit: canEditProp = true,
  anchorId,
  onConnectionSaved,
  onFirstChatModel,
}: AzureFoundryPanelProps) {
  const {t} = useTranslation('settings')
  const {confirm} = useConfirm()
  // One object per scope, whatever the host passes each render: the store's
  // selectors and the children's effects key on it.
  const orgId = scopeProp.kind === 'org' ? scopeProp.orgId : null
  const scope = useMemo<AzureScope>(() => (orgId ? {kind: 'org', orgId} : {kind: 'user'}), [orgId])
  const state = useAzureFoundryStore(useMemo(() => selectAzureScope(scope), [scope]))
  const load = useAzureFoundryStore(s => s.load)
  const runTest = useAzureFoundryStore(s => s.test)
  const createModel = useAzureFoundryStore(s => s.createModel)
  const updateModel = useAzureFoundryStore(s => s.updateModel)
  const deleteModel = useAzureFoundryStore(s => s.deleteModel)
  const deleteConnection = useAzureFoundryStore(s => s.deleteConnection)
  const dismissDiscovery = useAzureFoundryStore(s => s.dismissDiscovery)
  const savedDefault = useSettingsStore(s => s.settings.ai.providers['azure-foundry']?.defaultModel)
  const systemInfo = useSystemStore(s => s.info)
  const localBackend = systemInfo ? systemInfo.capabilities.sessionAnalytics : isTauri()
  const anchor = useUIStore(s => s.settingsAnchor)

  const labelOf = useCallback((p: ModelPurposePreset) => t(purposeLabelKey(p), {ns: 'common'}), [t])

  // ---- loading ----

  useEffect(() => {
    void load(scope)
  }, [load, scope])
  // A view marked stale while on screen (a lender's credential disabled, a
  // change made elsewhere) is fetched again, in the background.
  useEffect(() => {
    if (state.status === 'ready' && state.loadedAt === 0) void load(scope)
  }, [state.status, state.loadedAt, load, scope])

  const unavailable = state.errorKind === 'unavailable'
  const onUnavailableRef = useRef(onUnavailable)
  useEffect(() => {
    onUnavailableRef.current = onUnavailable
  })
  useEffect(() => {
    if (unavailable) onUnavailableRef.current?.()
  }, [unavailable])

  // ---- what the scope holds ----

  const readOnlyRegistry = state.registryState === 'read_only'
  const canEdit = canEditProp && !readOnlyRegistry
  // An Azure knowledge base exists (cloud, with Azure embeddings): only then
  // does the knowledge-base designation show or count — never on desktop.
  const knowledgeEnabled = state.knowledgeEmbeddingEnabled
  const ownConnections = useMemo(() => state.connections.filter(c => isOwnItem(scope, c)), [state.connections, scope])
  const ownModels = useMemo(() => state.models.filter(m => isOwnItem(scope, m)), [state.models, scope])
  const shared = useMemo(
    () => (scope.kind === 'user' ? sharedGroups(state.connections, state.models) : []),
    [scope, state.connections, state.models],
  )
  const limits = limitsView(ownConnections.length, ownModels.length, state.limits)
  const defaultModelId =
    scope.kind === 'user' ? effectiveDefaultModelId(savedDefault, state.serverDefault, state.models) : null
  const allowCli = scope.kind === 'user' && localBackend

  // ---- live regions ----

  const [status, setStatus] = useState({text: '', n: 0})
  const [alert, setAlert] = useState<{text: string; n: number} | null>(null)
  const announce = useCallback((text: string) => setStatus(s => ({text, n: s.n + 1})), [])
  const raise = useCallback((text: string) => setAlert(a => ({text, n: (a?.n ?? 0) + 1})), [])

  // Why an action failed: a 403 and a read-only desktop list file get local
  // copy; anything else keeps the server's own message.
  const reasonOf = useCallback(
    (err: unknown) =>
      err instanceof ApiError && err.status === 403
        ? t('azureModels.conflict.readOnly')
        : err instanceof ApiError && err.code === 'REGISTRY_READ_ONLY'
          ? t('azureModels.registryState.readOnly')
          : err instanceof Error && err.message
            ? err.message
            : t('azureModels.section.requestFailed'),
    [t],
  )

  // ---- focus ----
  //
  // After an action focus goes to a data-focus target of the render that
  // follows it (the store was patched before the action resolved, so the
  // target is there). This runs after a closing modal's own focus restore:
  // effect cleanups run before effects.

  const rootRef = useRef<HTMLDivElement>(null)
  const [pendingFocus, setPendingFocus] = useState<string | null>(null)
  useEffect(() => {
    if (!pendingFocus) return
    const target = [...(rootRef.current?.querySelectorAll<HTMLElement>('[data-focus]') ?? [])].find(
      el => el.dataset.focus === pendingFocus,
    )
    target?.focus()
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot: cleared once used
    setPendingFocus(null)
  }, [pendingFocus])

  // A Settings deep link to the host's heading: focused once the data has
  // loaded (after Settings' own scroll reset), then the anchor is spent.
  const settled = state.status === 'ready' || state.status === 'error'
  useEffect(() => {
    if (!anchorId || anchor !== anchorId || !settled) return
    document.getElementById(anchorId)?.focus()
    useUIStore.getState().clearSettingsAnchor()
  }, [anchor, anchorId, settled])

  // ---- modals ----

  const [connectionModal, setConnectionModal] = useState<ConnectionModalState | null>(null)
  const [modelModal, setModelModal] = useState<ModelModalState | null>(null)
  // The resource just added: its discovery suggestions open at once.
  const [justAdded, setJustAdded] = useState<string | null>(null)

  // Focus goes to the resource's heading after any save: a new resource's is
  // where it now is, and the button that opened the modal for a saved one
  // may be gone — "Enter credentials" and the invalid-settings callout's
  // "Edit resource" leave with the callout once the resource is fixed, which
  // would drop focus to the page.
  const handleConnectionSaved = (connection: AzureConnection, created: boolean) => {
    setConnectionModal(null)
    announce(
      t(created ? 'azureModels.announce.resourceAdded' : 'azureModels.announce.resourceSaved', {
        name: connection.name,
      }),
    )
    if (created) setJustAdded(connection.id)
    setPendingFocus(focusKey.connection(connection.id))
    onConnectionSaved?.(connection)
  }

  // The scope's first chat model: nothing else of a chat kind is held now.
  const reportFirstChatModel = (entry: AzureModelEntry) => {
    if (!onFirstChatModel || !isChatKind(entry.kind)) return
    const others = selectAzureScope(scope)(useAzureFoundryStore.getState()).models.filter(
      m => isOwnItem(scope, m) && m.id !== entry.id && isChatKind(m.kind),
    )
    if (others.length === 0) onFirstChatModel()
  }

  const handleModelSaved = (entry: AzureModelEntry, created: boolean) => {
    setModelModal(null)
    announce(
      t(created ? 'azureModels.announce.modelAdded' : 'azureModels.announce.modelSaved', {name: entry.displayName}),
    )
    if (created) {
      setPendingFocus(focusKey.modelName(entry.id))
      reportFirstChatModel(entry)
    }
  }

  const confirmReindex = () =>
    confirm({
      title: t('azureModels.knowledge.reindexTitle'),
      message: t('azureModels.knowledge.reindexMessage'),
      confirmLabel: t('azureModels.knowledge.reindexConfirm'),
    })

  // ---- resource actions ----

  const testConnection = async (connection: AzureConnection) => {
    try {
      const result = await runTest(scope, {connectionId: connection.id})
      if (result.ok) {
        announce(t('azureModels.announce.testPassed', {name: connection.name, ms: formatCount(result.latencyMs)}))
      } else {
        raise(
          t('azureModels.announce.testFailed', {
            name: connection.name,
            reason: result.error || t('azureModels.test.failedGeneric'),
          }),
        )
      }
    } catch (err) {
      raise(t('azureModels.announce.testRequestFailed', {name: connection.name, reason: reasonOf(err)}))
    }
  }

  const removeConnection = async (connection: AzureConnection) => {
    const models = ownModels.filter(m => m.connectionId === connection.id)
    // The knowledge-base model matters only where an Azure knowledge base exists.
    const knowledge = knowledgeEnabled ? models.find(m => m.isKnowledgeEmbedding) : undefined
    const ok = await confirm({
      title: t('azureModels.remove.resourceTitle', {name: connection.name}),
      message: (
        <>
          <p>
            {models.length > 0
              ? t('azureModels.remove.resourceMessage', {
                  count: models.length,
                  models: models.map(m => m.displayName).join(', '),
                })
              : t('azureModels.remove.resourceMessageEmpty')}
          </p>
          {knowledge && <p className="mt-2">{t('azureModels.remove.knowledgeNote', {name: knowledge.displayName})}</p>}
        </>
      ),
      danger: true,
      confirmLabel:
        models.length > 0
          ? t('azureModels.remove.resourceConfirm', {count: models.length})
          : t('azureModels.remove.resourceConfirmEmpty'),
    })
    if (!ok) return
    const next = focusAfterConnectionRemoval(
      ownConnections.map(c => c.id),
      connection.id,
    )
    // Sent without force: only a corpus that holds documents needs the
    // re-index, and the server says so (REINDEX_REQUIRED). The confirm above
    // already warned about it when the resource holds the knowledge-base
    // model, so that answer is confirmed at once instead of asking twice.
    const run = async (force: boolean): Promise<void> => {
      try {
        await deleteConnection(scope, connection.id, {cascade: models.length > 0, ...(force ? {force: true} : {})})
        announce(t('azureModels.announce.resourceRemoved', {name: connection.name}))
        setPendingFocus(next)
      } catch (err) {
        if (!force && isCode(err, 'REINDEX_REQUIRED')) {
          if (knowledge || (await confirmReindex())) await run(true)
          return
        }
        // Models this view did not know of: show them.
        if (isCode(err, 'CONNECTION_IN_USE')) void load(scope, {force: true})
        raise(t('azureModels.announce.removeFailed', {name: connection.name, reason: reasonOf(err)}))
      }
    }
    await run(false)
  }

  const dismissSuggestions = async (connection: AzureConnection) => {
    try {
      await dismissDiscovery(scope, connection.id)
      announce(t('azureModels.announce.suggestionsHidden', {name: connection.name}))
      setPendingFocus(focusKey.discoveryToggle(connection.id))
    } catch (err) {
      raise(t('azureModels.announce.dismissFailed', {reason: reasonOf(err)}))
    }
  }

  // A refusal is raised as an alert as well as shown on the item's row (the
  // row is no live region).
  const quickAdd = async (connection: AzureConnection, item: AzureDiscoveryItem): Promise<QuickAddResult> => {
    try {
      const entry = await createModel(scope, discoveryQuickAddInput(item, connection.id))
      announce(t('azureModels.announce.modelAdded', {name: entry.displayName}))
      reportFirstChatModel(entry)
      return {ok: true}
    } catch (err) {
      const reason = reasonOf(err)
      raise(t('azureModels.announce.addFailed', {name: item.name, reason}))
      return {ok: false, error: reason}
    }
  }

  // One after the other (the limit is checked per add); at the limit the
  // rest are refused with the same reason.
  const addAll = async (connection: AzureConnection, items: AzureDiscoveryItem[]) => {
    const failures: Record<string, string> = {}
    let added = 0
    for (let i = 0; i < items.length; i++) {
      const item = items[i]
      if (!item) continue
      try {
        const entry = await createModel(scope, discoveryQuickAddInput(item, connection.id))
        added++
        reportFirstChatModel(entry)
      } catch (err) {
        const reason = reasonOf(err)
        failures[discoveryItemKey(item)] = reason
        if (isCode(err, 'TOO_MANY_MODELS')) {
          for (const rest of items.slice(i + 1)) failures[discoveryItemKey(rest)] = reason
          break
        }
      }
    }
    if (added > 0) announce(t('azureModels.announce.addedMany', {count: added}))
    const failed = Object.keys(failures).length
    if (failed > 0) raise(t('azureModels.announce.notAdded', {count: failed}))
    return failures
  }

  const connectionActions: ConnectionActions = {
    onAddModel: connection => setModelModal({entry: null, initial: emptyModelForm(connection.id)}),
    onEdit: connection => setConnectionModal({mode: 'edit', connection}),
    onEnterCredentials: connection => setConnectionModal({mode: 'reauth', connection}),
    onTest: connection => void testConnection(connection),
    onRemove: connection => void removeConnection(connection),
    onQuickAdd: quickAdd,
    onAddAll: addAll,
    onAddWithDetails: (connection, item) =>
      setModelModal({
        entry: null,
        initial: modelFormFromDiscovery(item, connection.id),
        reportedWindow: item.contextWindow,
      }),
    onDismissDiscovery: connection => void dismissSuggestions(connection),
  }

  // ---- model actions ----

  const testModel = async (entry: AzureModelEntry) => {
    try {
      const result = await runTest(scope, {model: {id: entry.id}})
      if (result.ok) {
        announce(t('azureModels.announce.testPassed', {name: entry.displayName, ms: formatCount(result.latencyMs)}))
      } else {
        raise(
          t('azureModels.announce.testFailed', {
            name: entry.displayName,
            reason: result.error || t('azureModels.test.failedGeneric'),
          }),
        )
      }
    } catch (err) {
      raise(t('azureModels.announce.testRequestFailed', {name: entry.displayName, reason: reasonOf(err)}))
    }
  }

  // "Default for new chats" is a setting, not a registry change: optimistic,
  // and on a failed save only that field goes back.
  const makeDefault = async (entry: AzureModelEntry) => {
    announce(t('azureModels.announce.defaultSet', {name: entry.displayName}))
    const saved = await useSettingsStore.getState().updateProvider('azure-foundry', {defaultModel: entry.modelId})
    if (saved === false) raise(t('azureModels.announce.defaultFailed', {name: entry.displayName}))
  }

  const makeOrgDefault = async (entry: AzureModelEntry) => {
    try {
      await updateModel(scope, entry.id, {isDefault: true})
      announce(t('azureModels.announce.orgDefaultSet', {name: entry.displayName}))
    } catch (err) {
      raise(t('azureModels.announce.updateFailed', {name: entry.displayName, reason: reasonOf(err)}))
    }
  }

  const designateKnowledgeModel = async (entry: AzureModelEntry, force = false): Promise<void> => {
    try {
      await updateModel(scope, entry.id, {isKnowledgeEmbedding: true, ...(force ? {force: true} : {})})
      announce(
        t(force ? 'azureModels.announce.knowledgeSetReindex' : 'azureModels.announce.knowledgeSet', {
          name: entry.displayName,
        }),
      )
    } catch (err) {
      if (!force && isCode(err, 'REINDEX_REQUIRED')) {
        if (await confirmReindex()) await designateKnowledgeModel(entry, true)
        return
      }
      raise(t('azureModels.announce.updateFailed', {name: entry.displayName, reason: reasonOf(err)}))
    }
  }

  const removeModel = async (entry: AzureModelEntry) => {
    const knowledge = knowledgeEnabled && entry.isKnowledgeEmbedding
    const ok = await confirm({
      title: t('azureModels.remove.modelTitle', {name: entry.displayName}),
      message: (
        <>
          <p>{t('azureModels.remove.modelMessage', {name: entry.displayName})}</p>
          {knowledge && <p className="mt-2">{t('azureModels.remove.knowledgeNote', {name: entry.displayName})}</p>}
        </>
      ),
      danger: true,
      confirmLabel: t('azureModels.remove.modelConfirm'),
    })
    if (!ok) return
    const next = focusAfterModelRemoval(
      ownModels.filter(m => m.connectionId === entry.connectionId).map(m => m.id),
      entry.id,
      entry.connectionId,
    )
    // As for a resource: force only once the server says the corpus needs
    // the re-index, and without a second question when the confirm said so.
    const run = async (force: boolean): Promise<void> => {
      try {
        await deleteModel(scope, entry.id, force ? {force: true} : {})
        announce(t('azureModels.announce.modelRemoved', {name: entry.displayName}))
        setPendingFocus(next)
      } catch (err) {
        if (!force && isCode(err, 'REINDEX_REQUIRED')) {
          if (knowledge || (await confirmReindex())) await run(true)
          return
        }
        raise(t('azureModels.announce.removeFailed', {name: entry.displayName, reason: reasonOf(err)}))
      }
    }
    await run(false)
  }

  const rowActions: ModelRowActions = {
    onEdit: entry =>
      setModelModal({entry, initial: modelFormFromEntry(entry), reportedWindow: entry.reportedContextWindow}),
    onTest: entry => void testModel(entry),
    ...(scope.kind === 'org'
      ? {onMakeOrgDefault: (entry: AzureModelEntry) => void makeOrgDefault(entry)}
      : {onMakeDefault: (entry: AzureModelEntry) => void makeDefault(entry)}),
    onUseForKnowledge: entry => void designateKnowledgeModel(entry),
    onRemove: entry => void removeModel(entry),
  }

  // ---- render ----

  const uid = useId()
  const limitsId = `${uid}-limits`
  const connectionsLimitId = `${uid}-connections-limit`
  const modelsLimitId = `${uid}-models-limit`
  const modelLimitReason = limits.models.reached
    ? t('azureModels.limits.modelsReached', {count: limits.models.limit})
    : undefined

  if (unavailable) return null

  const loading = state.status === 'idle' || state.status === 'loading'
  const loadAlert =
    state.errorKind === 'failed'
      ? state.status === 'ready'
        ? t('azureModels.section.refreshFailed')
        : t('azureModels.section.loadFailed')
      : null
  const forbidden = state.status === 'error' && state.errorKind === 'forbidden'
  const ready = state.status === 'ready'

  return (
    <div ref={rootRef}>
      <div role="status" className="sr-only">
        {status.text && <span key={status.n}>{status.text}</span>}
      </div>
      <div className={clsx('flex items-start gap-2', (loadAlert || alert) && 'mb-4')}>
        <div
          role="alert"
          className={clsx(
            'min-w-0 flex-1',
            (loadAlert || alert) &&
              'space-y-1 rounded-lg border border-semantic-error/30 bg-semantic-error/10 px-3 py-2 text-sm text-text-primary',
          )}
        >
          {loadAlert && <p>{loadAlert}</p>}
          {alert && <p key={alert.n}>{alert.text}</p>}
        </div>
        {loadAlert && (
          <Button size="sm" variant="secondary" onClick={() => void load(scope, {force: true})}>
            {t('azureModels.section.retry')}
          </Button>
        )}
        {alert && (
          <IconButton icon={X} size="sm" label={t('azureModels.section.dismiss')} onClick={() => setAlert(null)} />
        )}
      </div>

      {state.registryState === 'read_only' && <Notice>{t('azureModels.registryState.readOnly')}</Notice>}
      {state.registryState === 'recovered' && (
        <Notice>
          {state.recoveredCopyPath
            ? t('azureModels.registryState.recovered', {path: state.recoveredCopyPath})
            : t('azureModels.registryState.recoveredNoPath')}
        </Notice>
      )}
      {scope.kind === 'org' && !canEditProp && ready && (
        <p className="mb-3 text-xs text-text-tertiary">{t('azureModels.section.readOnlyOrg')}</p>
      )}

      {canEdit && ready && ownConnections.length > 0 && (
        <div className="mb-4 space-y-1">
          <div className="flex flex-wrap items-center justify-end gap-2">
            {limits.show && (
              <p id={limitsId} className="mr-auto text-xs text-text-tertiary">
                {t('azureModels.limits.both', {
                  connections: t('azureModels.limits.connections', {
                    used: limits.connections.used,
                    count: limits.connections.limit,
                  }),
                  models: t('azureModels.limits.models', {used: limits.models.used, count: limits.models.limit}),
                })}
              </p>
            )}
            <Button
              size="sm"
              variant="secondary"
              icon={Plus}
              onClick={() => setModelModal({entry: null, initial: emptyModelForm(ownConnections[0]?.id ?? '')})}
              disabled={limits.models.reached}
              aria-describedby={limits.models.reached ? modelsLimitId : undefined}
            >
              {t('azureModels.section.addModel')}
            </Button>
            <Button
              size="sm"
              variant="secondary"
              onClick={() => setConnectionModal({mode: 'add', connection: null})}
              disabled={limits.connections.reached}
              aria-describedby={limits.connections.reached ? connectionsLimitId : undefined}
              data-focus={focusKey.addResource}
            >
              {t('azureModels.section.addResource')}
            </Button>
          </div>
          {limits.connections.reached && (
            <p id={connectionsLimitId} className="text-right text-xs text-text-secondary">
              {t('azureModels.limits.connectionsReached', {count: limits.connections.limit})}
            </p>
          )}
          {limits.models.reached && (
            <p id={modelsLimitId} className="text-right text-xs text-text-secondary">
              {modelLimitReason}
            </p>
          )}
        </div>
      )}

      {loading ? (
        <div aria-busy="true" className="rounded-lg border border-border-default bg-surface-1 p-4">
          <span className="sr-only">{t('azureModels.section.loading')}</span>
          <Skeleton lines={3} className="max-w-md" />
        </div>
      ) : forbidden ? (
        <p className="text-sm text-text-secondary">{t('azureModels.section.noAccess')}</p>
      ) : ready ? (
        <div className="space-y-4">
          {ownConnections.length === 0 &&
            (canEdit ? (
              <EmptyState
                icon={Cloud}
                headingLevel={headingLevel}
                title={t('azureModels.empty.title')}
                description={
                  scope.kind === 'org' ? t('azureModels.empty.orgDescription') : t('azureModels.empty.description')
                }
                className="rounded-lg border border-dashed border-border-default py-8!"
                action={
                  <Button
                    variant="primary"
                    onClick={() => setConnectionModal({mode: 'add', connection: null})}
                    data-focus={focusKey.addResource}
                  >
                    {t('azureModels.section.addResource')}
                  </Button>
                }
              >
                <ul className="list-disc space-y-1 pl-4 text-left text-xs text-text-tertiary">
                  <li>{t('azureModels.empty.needEndpoint')}</li>
                  <li>{t('azureModels.empty.needCredentials')}</li>
                  <li>{t('azureModels.empty.needModels')}</li>
                </ul>
              </EmptyState>
            ) : scope.kind === 'org' ? (
              <p className="text-sm text-text-secondary">{t('azureModels.empty.orgMember')}</p>
            ) : null)}
          {ownConnections.map(connection => (
            <AzureConnectionGroup
              key={connection.id}
              scope={scope}
              connection={connection}
              models={ownModels.filter(m => m.connectionId === connection.id)}
              headingLevel={headingLevel}
              canEdit={canEdit}
              defaultModelId={defaultModelId}
              knowledgeEnabled={knowledgeEnabled}
              expandDiscovery={justAdded === connection.id}
              modelLimitReason={modelLimitReason}
              busy={state.busy}
              actions={connectionActions}
              rowActions={rowActions}
              labelOf={labelOf}
            />
          ))}
          <AzureSharedModels
            groups={shared}
            headingLevel={headingLevel}
            defaultModelId={defaultModelId}
            knowledgeEnabled={knowledgeEnabled}
            labelOf={labelOf}
          />
        </div>
      ) : null}

      {connectionModal && (
        <AzureConnectionModal
          scope={scope}
          mode={connectionModal.mode}
          connection={connectionModal.connection}
          allowCli={allowCli}
          onClose={() => setConnectionModal(null)}
          onSaved={handleConnectionSaved}
        />
      )}
      {modelModal && (
        <AzureModelModal
          scope={scope}
          entry={modelModal.entry}
          initial={modelModal.initial}
          reportedWindow={modelModal.reportedWindow}
          connections={ownConnections}
          models={ownModels}
          onClose={() => setModelModal(null)}
          onSaved={handleModelSaved}
        />
      )}
    </div>
  )
}

// A note about the whole list (the desktop list file's state).
function Notice({children}: {children: React.ReactNode}) {
  return (
    <div className="mb-4 flex items-start gap-2 rounded-lg border border-semantic-info/30 bg-semantic-info/10 px-3 py-2 text-xs text-text-secondary">
      <Info size={14} aria-hidden="true" className="mt-0.5 shrink-0 text-semantic-info" />
      <p className="min-w-0 wrap-break-word">{children}</p>
    </div>
  )
}
