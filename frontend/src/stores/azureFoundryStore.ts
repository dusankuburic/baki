import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import {useChatStore} from './chatStore'
import {providersApi} from '@/api'
import {ApiError, VersionConflictError} from '@/api/client'
import {compareRegistryModels} from '@/lib/modelGroups'
import {logger} from '@/lib/logger'
import type {
  AzureConnection,
  AzureConnectionCreateInput,
  AzureConnectionDeleteOptions,
  AzureConnectionDeleteResult,
  AzureConnectionUpdateInput,
  AzureDiscovery,
  AzureDiscoveryItem,
  AzureModelCreateInput,
  AzureModelDeleteOptions,
  AzureModelEntry,
  AzureModelKind,
  AzureModelsListing,
  AzureModelUpdateInput,
  AzureRegistryLimits,
  AzureRegistryState,
  AzureScope,
  AzureTestInput,
  AzureTestResult,
  Tolerant,
} from '@/types'

export const AZURE_REVALIDATE_MS = 60_000
export const AZURE_DISCOVERY_TTL_MS = 5 * 60_000

const DEFAULT_LIMITS: AzureRegistryLimits = {connections: 10, models: 50}

export type AzureLoadStatus = 'idle' | 'loading' | 'ready' | 'error'
export type AzureLoadErrorKind = 'failed' | 'forbidden' | 'unavailable'

export interface AzureDiscoveryState {
  status: 'loading' | 'ready' | 'error'
  data: AzureDiscovery | null
  fetchedAt: number
  error: string | null
  errorCode: string | null
}

export interface AzureScopeState {
  status: AzureLoadStatus
  errorKind: AzureLoadErrorKind | null
  error: string | null
  connections: AzureConnection[]
  models: AzureModelEntry[]
  registryState: Tolerant<AzureRegistryState>
  recoveredCopyPath: string | null
  limits: AzureRegistryLimits
  knowledgeEmbeddingEnabled: boolean
  serverDefault: string | null
  discovery: Record<string, AzureDiscoveryState>
  busy: Record<string, true>
  loadedAt: number
}

export const EMPTY_AZURE_SCOPE: AzureScopeState = {
  status: 'idle',
  errorKind: null,
  error: null,
  connections: [],
  models: [],
  registryState: 'ok',
  recoveredCopyPath: null,
  limits: DEFAULT_LIMITS,
  knowledgeEmbeddingEnabled: false,
  serverDefault: null,
  discovery: {},
  busy: {},
  loadedAt: 0,
}

const NO_DISCOVERY: AzureDiscoveryState = {status: 'loading', data: null, fetchedAt: 0, error: null, errorCode: null}

export function azureScopeKey(scope: AzureScope): string {
  return scope.kind === 'org' ? `org:${scope.orgId}` : 'user'
}

function savedTestTarget(input: AzureTestInput): {kind: 'model' | 'connection'; id: string} | null {
  if (input.config) return null
  if (input.model) return 'id' in input.model ? {kind: 'model', id: input.model.id} : null
  return input.connectionId ? {kind: 'connection', id: input.connectionId} : null
}

export const azureBusyKey = {
  createConnection: 'connection:new',
  connection: (id: string) => `connection:${id}`,
  createModel: 'model:new',
  model: (id: string) => `model:${id}`,
  discover: (connectionId: string) => `discover:${connectionId}`,
  test: (input: AzureTestInput): string => {
    const target = savedTestTarget(input)
    return target ? `test:${target.kind}:${target.id}` : 'test:draft'
  },
}

export class AzureConflictError extends VersionConflictError {
  constructor(cause: ApiError) {
    super(cause.message, cause.code, cause.requestId, cause.details)
    this.name = 'AzureConflictError'
  }
}

export interface AzureFoundryState {
  scopes: Record<string, AzureScopeState>
  knowledgeReindex: Record<string, true>

  load: (scope: AzureScope, opts?: {force?: boolean}) => Promise<void>
  invalidate: (scope?: AzureScope) => void
  createConnection: (scope: AzureScope, input: AzureConnectionCreateInput) => Promise<AzureConnection>
  updateConnection: (scope: AzureScope, id: string, input: AzureConnectionUpdateInput) => Promise<AzureConnection>
  dismissDiscovery: (scope: AzureScope, id: string) => Promise<AzureConnection>
  deleteConnection: (
    scope: AzureScope,
    id: string,
    opts?: AzureConnectionDeleteOptions,
  ) => Promise<AzureConnectionDeleteResult>
  discover: (scope: AzureScope, connectionId: string, opts?: {force?: boolean}) => Promise<AzureDiscovery>
  createModel: (scope: AzureScope, input: AzureModelCreateInput) => Promise<AzureModelEntry>
  updateModel: (scope: AzureScope, id: string, input: AzureModelUpdateInput) => Promise<AzureModelEntry>
  deleteModel: (scope: AzureScope, id: string, opts?: AzureModelDeleteOptions) => Promise<void>
  test: (scope: AzureScope, input: AzureTestInput) => Promise<AzureTestResult>
  clearKnowledgeReindex: (scopes?: AzureScope[]) => void
}

const loadsInFlight = new Map<string, Promise<void>>()
const discoveriesInFlight = new Map<string, Promise<AzureDiscovery>>()
const mutationGen = new Map<string, number>()
const defaultReads = new Map<string, {again: boolean}>()
const loadsApplied = new Map<string, number>()

const DEFAULT_FIELDS = ['connectionId', 'kind', 'deployment', 'displayName', 'purpose', 'purposeCustom'] as const
let sessionEpoch = 0

type Owned = {shared?: boolean; orgId?: string}

function ownerKey(item: Owned): string {
  return item.shared ? `org:${item.orgId ?? ''}` : 'self'
}

function ownedHere(scope: AzureScope, item: Owned): boolean {
  return scope.kind === 'org' || !item.shared
}

function sameConnection(a: AzureConnection, b: AzureConnection): boolean {
  return a.id === b.id && ownerKey(a) === ownerKey(b)
}

function sameModel(a: AzureModelEntry, b: AzureModelEntry): boolean {
  return a.id === b.id && ownerKey(a) === ownerKey(b)
}

function upsert<T>(items: T[], item: T, same: (a: T, b: T) => boolean): T[] {
  return items.some(x => same(x, item)) ? items.map(x => (same(x, item) ? item : x)) : [...items, item]
}

function sortModels(models: AzureModelEntry[]): AzureModelEntry[] {
  return [...models].sort((a, b) => compareRegistryModels(a, b))
}

function applyDesignations(models: AzureModelEntry[], changed: AzureModelEntry): AzureModelEntry[] {
  if (!changed.isDefault && !changed.isKnowledgeEmbedding) return models
  return models.map(m => {
    if (sameModel(m, changed) || ownerKey(m) !== ownerKey(changed)) return m
    const clearDefault = changed.isDefault && m.isDefault
    const clearKb = changed.isKnowledgeEmbedding && m.isKnowledgeEmbedding
    return clearDefault || clearKb
      ? {...m, ...(clearDefault ? {isDefault: false} : {}), ...(clearKb ? {isKnowledgeEmbedding: false} : {})}
      : m
  })
}

function recount(connections: AzureConnection[], models: AzureModelEntry[]): AzureConnection[] {
  return connections.map(c => {
    const count = models.filter(m => m.connectionId === c.id && ownerKey(m) === ownerKey(c)).length
    return count === c.modelCount ? c : {...c, modelCount: count}
  })
}

function discoveryKey(kind: Tolerant<AzureModelKind>, name: string): string {
  return `${kind === 'agent' ? 'agent' : 'deployment'}\u0000${name.toLowerCase()}`
}

function reconcileDiscovery(
  scope: AzureScope,
  connectionId: string,
  data: AzureDiscovery,
  models: AzureModelEntry[],
): AzureDiscovery {
  const held = new Map<string, AzureModelEntry>()
  for (const m of models) {
    if (m.connectionId === connectionId && ownedHere(scope, m)) held.set(discoveryKey(m.kind, m.deployment), m)
  }
  let changed = false
  const fix = (item: AzureDiscoveryItem): AzureDiscoveryItem => {
    const entry = held.get(discoveryKey(item.kind, item.name))
    if (item.alreadyAdded === !!entry && item.modelId === entry?.modelId) return item
    changed = true
    const {modelId: _stale, ...rest} = item
    return entry ? {...rest, alreadyAdded: true, modelId: entry.modelId} : {...rest, alreadyAdded: false}
  }
  const deployments = data.deployments.map(fix)
  const agents = data.agents.map(fix)
  return changed ? {...data, deployments, agents} : data
}

function reconcileAllDiscovery(
  scope: AzureScope,
  discovery: Record<string, AzureDiscoveryState>,
  models: AzureModelEntry[],
): Record<string, AzureDiscoveryState> {
  let out = discovery
  for (const [id, d] of Object.entries(discovery)) {
    if (!d.data) continue
    const data = reconcileDiscovery(scope, id, d.data, models)
    if (data !== d.data) out = {...out, [id]: {...d, data}}
  }
  return out
}

function withoutKey<T>(record: Record<string, T>, key: string): Record<string, T> {
  if (!(key in record)) return record
  const out = {...record}
  delete out[key]
  return out
}

function loadErrorKind(err: unknown): AzureLoadErrorKind {
  if (err instanceof ApiError) {
    if (err.code === 'ORG_KEYSTORE_UNAVAILABLE') return 'unavailable'
    if (err.status === 403) return 'forbidden'
  }
  return 'failed'
}

function errorMessage(err: unknown): string {
  return err instanceof Error && err.message ? err.message : 'Request failed'
}

function isConflict(err: unknown): err is ApiError {
  return err instanceof ApiError && err.status === 409 && err.code === 'CONFLICT'
}

export const useAzureFoundryStore = create<AzureFoundryState>((set, get) => {
  const scopeState = (key: string): AzureScopeState => get().scopes[key] ?? EMPTY_AZURE_SCOPE

  const patchScope = (key: string, patch: (s: AzureScopeState) => Partial<AzureScopeState>) =>
    set(state => {
      const cur = state.scopes[key] ?? EMPTY_AZURE_SCOPE
      return {scopes: {...state.scopes, [key]: {...cur, ...patch(cur)}}}
    })

  const setBusy = (key: string, op: string, on: boolean) =>
    patchScope(key, s => ({busy: on ? {...s.busy, [op]: true} : withoutKey(s.busy, op)}))

  const markStale = (key: string) => {
    if (get().scopes[key]) patchScope(key, () => ({loadedAt: 0}))
  }

  const refreshServerDefault = (scope: AzureScope) => {
    if (scope.kind !== 'user') return
    const key = azureScopeKey(scope)
    const inFlight = defaultReads.get(key)
    if (inFlight) {
      inFlight.again = true
      return
    }
    const read = {again: false}
    defaultReads.set(key, read)
    const epoch = sessionEpoch
    const once = async (): Promise<void> => {
      const loadsBefore = loadsApplied.get(key) ?? 0
      let listing: AzureModelsListing | null = null
      try {
        listing = await new Promise<AzureModelsListing>(resolve => resolve(providersApi.listAzureModels(scope)))
      } catch (err) {
        logger.warn('Failed to refresh the Azure AI Foundry server default', err)
      }
      if (epoch !== sessionEpoch) return
      if (read.again) {
        read.again = false
        return once()
      }
      defaultReads.delete(key)
      if (listing && (loadsApplied.get(key) ?? 0) === loadsBefore) {
        const serverDefault = listing.serverDefault ?? null
        patchScope(key, () => ({serverDefault}))
      }
    }
    void once()
  }

  const mutate = async <T>(
    scope: AzureScope,
    op: string,
    run: () => Promise<T>,
    patch: (s: AzureScopeState, result: T) => Partial<AzureScopeState>,
    opts: {forced?: boolean; movesDefault?: boolean} = {},
  ): Promise<T> => {
    const key = azureScopeKey(scope)
    const epoch = sessionEpoch
    setBusy(key, op, true)
    try {
      const result = await run()
      if (epoch === sessionEpoch) {
        mutationGen.set(key, (mutationGen.get(key) ?? 0) + 1)
        patchScope(key, s => patch(s, result))
        if (opts.forced && scopeState(key).knowledgeEmbeddingEnabled) {
          set(state => ({knowledgeReindex: {...state.knowledgeReindex, [key]: true}}))
        }
        if (scope.kind === 'org') markStale(azureScopeKey({kind: 'user'}))
        if (opts.movesDefault) refreshServerDefault(scope)
        useChatStore.getState().bumpProviderEpoch()
      }
      return result
    } catch (err) {
      if (isConflict(err)) {
        if (epoch === sessionEpoch) markStale(key)
        throw new AzureConflictError(err)
      }
      if (err instanceof ApiError && err.code === 'REGISTRY_READ_ONLY' && epoch === sessionEpoch) markStale(key)
      throw err
    } finally {
      if (epoch === sessionEpoch) setBusy(key, op, false)
    }
  }

  const findConnection = (scope: AzureScope, s: AzureScopeState, id: string) =>
    s.connections.find(c => c.id === id && ownedHere(scope, c))

  const findModelEntry = (scope: AzureScope, s: AzureScopeState, id: string) =>
    s.models.find(m => m.id === id && ownedHere(scope, m))

  const withModels = (scope: AzureScope, s: AzureScopeState, models: AzureModelEntry[]): Partial<AzureScopeState> => {
    const sorted = sortModels(models)
    return {
      models: sorted,
      connections: recount(s.connections, sorted),
      discovery: reconcileAllDiscovery(scope, s.discovery, sorted),
    }
  }

  return {
    scopes: {},
    knowledgeReindex: {},

    load: (scope, opts = {}) => {
      const key = azureScopeKey(scope)
      const inFlight = loadsInFlight.get(key)
      if (inFlight) return inFlight
      const cur = scopeState(key)
      if (!opts.force && cur.status === 'ready' && Date.now() - cur.loadedAt < AZURE_REVALIDATE_MS) {
        return Promise.resolve()
      }
      const background = cur.status === 'ready'
      if (!background) patchScope(key, () => ({status: 'loading', errorKind: null, error: null}))
      const epoch = sessionEpoch
      const gen = mutationGen.get(key) ?? 0
      const promise: Promise<void> = Promise.all([
        providersApi.listAzureConnections(scope),
        providersApi.listAzureModels(scope),
      ])
        .then(([listing, modelsListing]) => {
          if (epoch !== sessionEpoch) return
          if ((mutationGen.get(key) ?? 0) !== gen) {
            loadsInFlight.delete(key)
            return get().load(scope, {force: true})
          }
          loadsApplied.set(key, (loadsApplied.get(key) ?? 0) + 1)
          const sorted = sortModels(modelsListing.models)
          const kept = (id: string) => listing.connections.some(c => c.id === id && ownedHere(scope, c))
          patchScope(key, s => ({
            status: 'ready',
            errorKind: null,
            error: null,
            connections: listing.connections,
            models: sorted,
            registryState: listing.registryState,
            recoveredCopyPath: listing.recoveredCopyPath ?? null,
            limits: listing.limits,
            knowledgeEmbeddingEnabled: listing.knowledgeEmbeddingEnabled,
            serverDefault: modelsListing.serverDefault ?? null,
            discovery: reconcileAllDiscovery(
              scope,
              Object.fromEntries(Object.entries(s.discovery).filter(([id]) => kept(id))),
              sorted,
            ),
            loadedAt: Date.now(),
          }))
        })
        .catch(err => {
          if (epoch !== sessionEpoch) return
          logger.warn('Failed to load the Azure AI Foundry registry', err)
          patchScope(key, s =>
            s.status === 'ready'
              ? {errorKind: loadErrorKind(err), error: errorMessage(err)}
              : {status: 'error', errorKind: loadErrorKind(err), error: errorMessage(err)},
          )
        })
      const tracked = promise.finally(() => {
        if (loadsInFlight.get(key) === tracked) loadsInFlight.delete(key)
      })
      loadsInFlight.set(key, tracked)
      return tracked
    },

    invalidate: scope => {
      if (scope) {
        markStale(azureScopeKey(scope))
        return
      }
      for (const key of Object.keys(get().scopes)) markStale(key)
    },

    createConnection: (scope, input) =>
      mutate(
        scope,
        azureBusyKey.createConnection,
        () => providersApi.createAzureConnection(scope, input),
        (s, conn) => ({connections: upsert(s.connections, conn, sameConnection)}),
      ),

    updateConnection: async (scope, id, input) => {
      const key = azureScopeKey(scope)
      const before = findConnection(scope, scopeState(key), id)
      const conn = await mutate(
        scope,
        azureBusyKey.connection(id),
        () => providersApi.updateAzureConnection(scope, id, input),
        (s, updated) => ({
          connections: recount(upsert(s.connections, updated, sameConnection), s.models),
          models: s.models.map(m =>
            m.connectionId === updated.id && ownerKey(m) === ownerKey(updated) && m.connectionName !== updated.name
              ? {...m, connectionName: updated.name}
              : m,
          ),
          discovery: input.config ? withoutKey(s.discovery, id) : s.discovery,
        }),
        {forced: input.force === true},
      )
      if (!before || before.status !== conn.status || before.supportsAgents !== conn.supportsAgents) {
        void get().load(scope, {force: true})
      }
      return conn
    },

    dismissDiscovery: (scope, id) => get().updateConnection(scope, id, {dismissDiscovery: true}),

    deleteConnection: (scope, id, opts = {}) =>
      mutate(
        scope,
        azureBusyKey.connection(id),
        () => providersApi.deleteAzureConnection(scope, id, opts),
        s => {
          const gone = (x: Owned & {id: string}) => x.id === id && ownedHere(scope, x)
          return {
            connections: s.connections.filter(c => !gone(c)),
            models: s.models.filter(m => !(m.connectionId === id && ownedHere(scope, m))),
            discovery: withoutKey(s.discovery, id),
          }
        },
        {forced: opts.force === true, movesDefault: true},
      ),

    discover: (scope, connectionId, opts = {}) => {
      const key = azureScopeKey(scope)
      const cached = scopeState(key).discovery[connectionId]
      if (
        !opts.force &&
        cached?.data &&
        !cached.data.error &&
        cached.status === 'ready' &&
        Date.now() - cached.fetchedAt < AZURE_DISCOVERY_TTL_MS
      ) {
        return Promise.resolve(cached.data)
      }
      const flightKey = `${key}\u0000${connectionId}`
      const inFlight = discoveriesInFlight.get(flightKey)
      if (inFlight) return inFlight
      const epoch = sessionEpoch
      const op = azureBusyKey.discover(connectionId)
      patchScope(key, s => ({
        busy: {...s.busy, [op]: true},
        discovery: {
          ...s.discovery,
          [connectionId]: {
            ...NO_DISCOVERY,
            ...s.discovery[connectionId],
            status: 'loading',
            error: null,
            errorCode: null,
          },
        },
      }))
      const promise = providersApi.discoverAzureConnection(scope, connectionId).then(
        raw => {
          if (epoch !== sessionEpoch) return raw
          let data = raw
          patchScope(key, s => {
            data = reconcileDiscovery(scope, connectionId, raw, s.models)
            return {
              busy: withoutKey(s.busy, op),
              discovery: {
                ...s.discovery,
                [connectionId]: {status: 'ready', data, fetchedAt: Date.now(), error: null, errorCode: null},
              },
            }
          })
          return data
        },
        err => {
          if (epoch === sessionEpoch) {
            patchScope(key, s => ({
              busy: withoutKey(s.busy, op),
              discovery: {
                ...s.discovery,
                [connectionId]: {
                  ...NO_DISCOVERY,
                  ...s.discovery[connectionId],
                  status: 'error',
                  error: errorMessage(err),
                  errorCode: err instanceof ApiError ? err.code : null,
                },
              },
            }))
          }
          throw err
        },
      )
      const tracked = promise.finally(() => {
        if (discoveriesInFlight.get(flightKey) === tracked) discoveriesInFlight.delete(flightKey)
      })
      discoveriesInFlight.set(flightKey, tracked)
      return tracked
    },

    createModel: (scope, input) =>
      mutate(
        scope,
        azureBusyKey.createModel,
        () => providersApi.createAzureModel(scope, input),
        (s, model) => withModels(scope, s, applyDesignations(upsert(s.models, model, sameModel), model)),
        {forced: input.force === true, movesDefault: true},
      ),

    updateModel: (scope, id, input) =>
      mutate(
        scope,
        azureBusyKey.model(id),
        () => providersApi.updateAzureModel(scope, id, input),
        (s, model) => withModels(scope, s, applyDesignations(upsert(s.models, model, sameModel), model)),
        {forced: input.force === true, movesDefault: DEFAULT_FIELDS.some(k => input[k] !== undefined)},
      ),

    deleteModel: async (scope, id, opts = {}) => {
      await mutate(
        scope,
        azureBusyKey.model(id),
        () => providersApi.deleteAzureModel(scope, id, opts),
        s => {
          const gone = findModelEntry(scope, s, id)
          return gone
            ? withModels(
                scope,
                s,
                s.models.filter(m => !sameModel(m, gone)),
              )
            : {}
        },
        {forced: opts.force === true, movesDefault: true},
      )
    },

    test: async (scope, input) => {
      const key = azureScopeKey(scope)
      const op = azureBusyKey.test(input)
      const target = savedTestTarget(input)
      const epoch = sessionEpoch
      setBusy(key, op, true)
      try {
        const result = await providersApi.testAzure(scope, input)
        if (epoch === sessionEpoch && target) {
          const lastTest = {
            ok: result.ok,
            latencyMs: result.latencyMs,
            ...(result.error ? {error: result.error} : {}),
            at: new Date().toISOString(),
          }
          const isTarget = (x: Owned & {id: string}) => x.id === target.id && ownedHere(scope, x)
          patchScope(key, s =>
            target.kind === 'model'
              ? {models: s.models.map(m => (isTarget(m) ? {...m, lastTest} : m))}
              : {connections: s.connections.map(c => (isTarget(c) ? {...c, lastTest} : c))},
          )
        }
        return result
      } finally {
        if (epoch === sessionEpoch) setBusy(key, op, false)
      }
    },

    clearKnowledgeReindex: scopes =>
      set(state => {
        if (!scopes) return Object.keys(state.knowledgeReindex).length > 0 ? {knowledgeReindex: {}} : {}
        let next = state.knowledgeReindex
        for (const s of scopes) next = withoutKey(next, azureScopeKey(s))
        return next === state.knowledgeReindex ? {} : {knowledgeReindex: next}
      }),
  }
})

export function selectAzureScope(scope: AzureScope): (state: AzureFoundryState) => AzureScopeState {
  const key = azureScopeKey(scope)
  return state => state.scopes[key] ?? EMPTY_AZURE_SCOPE
}

registerStoreReset(() => {
  sessionEpoch++
  loadsInFlight.clear()
  discoveriesInFlight.clear()
  mutationGen.clear()
  defaultReads.clear()
  loadsApplied.clear()
  useAzureFoundryStore.setState({scopes: {}, knowledgeReindex: {}})
})
