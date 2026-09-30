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
} from '@/types'

// The Azure AI Foundry model registry (resources — "connections" in the API
// — and their models), cached per scope: the user's own (plus, in user
// scope, the entries their organisations share with them) and each
// organisation's.
//
// - Reads dedupe while in flight; a view older than 60 s revalidates in the
//   background (the cached one stays on screen), and a failed load never
//   empties a view that loaded before.
// - Mutations patch the cache from the server's response, then bump the chat
//   store's provider epoch so the chat picker relists. A 409 CONFLICT
//   (compare-and-swap) rejects with AzureConflictError; every other failure
//   rejects with the ApiError as is (branch on `.code`).
// - In user scope a change of the models (or a resource removed with its
//   models) reads the model list again for its `serverDefault`, which no
//   mutation's response carries (see refreshServerDefault).
// - Discovery is cached per resource for 5 minutes and kept consistent with
//   the models, so `alreadyAdded` flips as models are added and removed.
//
// Ids are unique per owner only (a personal and an organisation's resource
// may both be `default`), so an item is matched by owner too: in user scope
// the scope's own items are the personal ones.

/** A loaded scope revalidates in the background once older than this. */
export const AZURE_REVALIDATE_MS = 60_000
/** Discovery results are reused for this long. */
export const AZURE_DISCOVERY_TTL_MS = 5 * 60_000

const DEFAULT_LIMITS: AzureRegistryLimits = {connections: 10, models: 50}

export type AzureLoadStatus = 'idle' | 'loading' | 'ready' | 'error'
/**
 * Why a load failed: `forbidden` — a 403 (the caller's org role may not
 * read it); `unavailable` — ORG_KEYSTORE_UNAVAILABLE (the deployment has no
 * org credentials; the host hides the section); `failed` — anything else.
 */
export type AzureLoadErrorKind = 'failed' | 'forbidden' | 'unavailable'

export interface AzureDiscoveryState {
  status: 'loading' | 'ready' | 'error'
  /** The last result (also kept while a refresh loads or after it failed). */
  data: AzureDiscovery | null
  /** When `data` arrived (ms epoch); 0 before the first result. */
  fetchedAt: number
  error: string | null
  errorCode: string | null
}

export interface AzureScopeState {
  status: AzureLoadStatus
  /** Why the last load failed. With status 'ready' a background refresh failed and the view is the cached one. */
  errorKind: AzureLoadErrorKind | null
  error: string | null
  connections: AzureConnection[]
  /** Sorted in the registry's display order (compareRegistryModels). */
  models: AzureModelEntry[]
  registryState: AzureRegistryState
  /** With registryState 'recovered': where the unreadable list file was kept, when the server says. */
  recoveredCopyPath: string | null
  limits: AzureRegistryLimits
  knowledgeEmbeddingEnabled: boolean
  /** User scope: the server's default for new chats ("m/<id>") — what applies while the user saved none. */
  serverDefault: string | null
  /** Per resource id (the scope's own resources). */
  discovery: Record<string, AzureDiscoveryState>
  /** Operations in flight — see azureBusyKey. */
  busy: Record<string, true>
  /** When the view last loaded (ms epoch); 0 = never, or invalidated. */
  loadedAt: number
}

/** The state of a scope nothing loaded yet — a stable reference for selectors. */
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

// A resource with no discovery result yet.
const NO_DISCOVERY: AzureDiscoveryState = {status: 'loading', data: null, fetchedAt: 0, error: null, errorCode: null}

/** The cache key of a scope. */
export function azureScopeKey(scope: AzureScope): string {
  return scope.kind === 'org' ? `org:${scope.orgId}` : 'user'
}

// The saved resource or model a test is about; null for a draft (settings
// sent along, or a model given by kind and deployment).
function savedTestTarget(input: AzureTestInput): {kind: 'model' | 'connection'; id: string} | null {
  if (input.config) return null
  if (input.model) return 'id' in input.model ? {kind: 'model', id: input.model.id} : null
  return input.connectionId ? {kind: 'connection', id: input.connectionId} : null
}

/** Keys of `busy`, so a row can disable its own buttons. */
export const azureBusyKey = {
  createConnection: 'connection:new',
  connection: (id: string) => `connection:${id}`,
  createModel: 'model:new',
  model: (id: string) => `model:${id}`,
  discover: (connectionId: string) => `discover:${connectionId}`,
  /** A saved resource's ("test:connection:<id>") or model's ("test:model:<id>") test; drafts share "test:draft". */
  test: (input: AzureTestInput): string => {
    const target = savedTestTarget(input)
    return target ? `test:${target.kind}:${target.id}` : 'test:draft'
  },
}

/**
 * A 409 CONFLICT: someone changed the resource (or the organisation's
 * credential) since it was read. Offer a reload — the scope is already
 * marked stale.
 */
export class AzureConflictError extends VersionConflictError {
  constructor(cause: ApiError) {
    super(cause.message, cause.code, cause.requestId, cause.details)
    this.name = 'AzureConflictError'
  }
}

export interface AzureFoundryState {
  /** Per azureScopeKey; read through selectAzureScope. */
  scopes: Record<string, AzureScopeState>
  /**
   * Scopes (azureScopeKey) where a confirmed re-index went through: a
   * knowledge-base embedding change sent with `force` where an Azure
   * knowledge base exists (knowledgeEmbeddingEnabled). The corpus it feeds
   * refuses uploads until it is re-indexed, so the Knowledge Base panel asks
   * for one. Cleared by clearKnowledgeReindex.
   */
  knowledgeReindex: Record<string, true>

  /** Loads the scope unless a fresh view is cached (see the header). `force` refetches regardless. Never rejects: the outcome is in the scope's state. */
  load: (scope: AzureScope, opts?: {force?: boolean}) => Promise<void>
  /** Marks a scope (or, without one, every scope) stale: the next load refetches. */
  invalidate: (scope?: AzureScope) => void
  createConnection: (scope: AzureScope, input: AzureConnectionCreateInput) => Promise<AzureConnection>
  updateConnection: (scope: AzureScope, id: string, input: AzureConnectionUpdateInput) => Promise<AzureConnection>
  /** Hides the resource's "Found in your project" prompt for good. */
  dismissDiscovery: (scope: AzureScope, id: string) => Promise<AzureConnection>
  deleteConnection: (
    scope: AzureScope,
    id: string,
    opts?: AzureConnectionDeleteOptions,
  ) => Promise<AzureConnectionDeleteResult>
  /** The resource's discoverable deployments and agents; cached for 5 minutes unless `force`. Rejects on a failed request. */
  discover: (scope: AzureScope, connectionId: string, opts?: {force?: boolean}) => Promise<AzureDiscovery>
  createModel: (scope: AzureScope, input: AzureModelCreateInput) => Promise<AzureModelEntry>
  updateModel: (scope: AzureScope, id: string, input: AzureModelUpdateInput) => Promise<AzureModelEntry>
  deleteModel: (scope: AzureScope, id: string, opts?: AzureModelDeleteOptions) => Promise<void>
  /** POST /test; a saved target's `lastTest` mirrors the result. */
  test: (scope: AzureScope, input: AzureTestInput) => Promise<AzureTestResult>
  /** The knowledge base was re-indexed: drops the suggestion for these scopes (every scope without any). */
  clearKnowledgeReindex: (scopes?: AzureScope[]) => void
}

// Loads and discoveries in flight, per scope key (and resource). Module
// state rather than store state: promises are not view state.
const loadsInFlight = new Map<string, Promise<void>>()
const discoveriesInFlight = new Map<string, Promise<AzureDiscovery>>()
// Bumped per scope by every successful mutation: a load that started before
// it may hold a pre-mutation view, so it reads again instead of applying.
const mutationGen = new Map<string, number>()
// Server-default reads in flight, per scope key (see refreshServerDefault):
// `again` asks for one more read once the one in flight answers.
const defaultReads = new Map<string, {again: boolean}>()
// Bumped per scope whenever a load applies. A load that lands while a
// server-default read is out started after every change before it (older
// ones read again, see mutationGen), so it is at least as fresh: that read's
// answer is dropped.
const loadsApplied = new Map<string, number>()

// The fields of a model the server default depends on: which entries are
// chat entries, their display order, and the legacy chat deployment.
const DEFAULT_FIELDS = ['connectionId', 'kind', 'deployment', 'displayName', 'purpose', 'purposeCustom'] as const
// Bumped on reset (logout): work finishing afterwards must not write the old
// session's data back.
let sessionEpoch = 0

// ---- pure cache patches ----

type Owned = {shared?: boolean; orgId?: string}

function ownerKey(item: Owned): string {
  return item.shared ? `org:${item.orgId ?? ''}` : 'self'
}

// The items a scope's own routes act on: every item in org scope, the
// personal ones in user scope (shared entries are read-only there).
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

// The designations are one per owner: a new default / knowledge-base model
// clears the owner's previous one.
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

// Each connection's modelCount recounted from the cached models of its owner.
function recount(connections: AzureConnection[], models: AzureModelEntry[]): AzureConnection[] {
  return connections.map(c => {
    const count = models.filter(m => m.connectionId === c.id && ownerKey(m) === ownerKey(c)).length
    return count === c.modelCount ? c : {...c, modelCount: count}
  })
}

// Discovery marks a deployment added when the resource holds an entry of any
// non-agent kind with its name, and an agent when it holds an agent entry of
// that name (names compared case-insensitively) — the server's rule.
function discoveryKey(kind: AzureModelKind, name: string): string {
  return `${kind === 'agent' ? 'agent' : 'deployment'}\u0000${name.toLowerCase()}`
}

// Sets `alreadyAdded` / `modelId` on a resource's discovery from the models
// the scope holds for it, so a quick add or a removal shows at once and a
// discovery that raced a mutation cannot bring back a stale mark.
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

  // A view older than now: the next load refetches (in the background when
  // one is cached).
  const markStale = (key: string) => {
    if (get().scopes[key]) patchScope(key, () => ({loadedAt: 0}))
  }

  // User scope: the model new chats start on while the user saved none
  // (serverDefault) follows the personal chat entries — the first usable one
  // in display order, else the organisation's default — so adding, removing
  // or changing a model, or removing a resource with its models, can move
  // it, and no mutation's response says where. The model list is read again
  // for it, and only serverDefault is taken (the models are already patched
  // from the response). One read in flight per scope: a change landing
  // meanwhile reads once more after it, since the answer may predate it.
  // A failed read keeps the last value.
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
        // (A promise executor, so even a synchronous throw only fails the read.)
        listing = await new Promise<AzureModelsListing>(resolve => resolve(providersApi.listAzureModels(scope)))
      } catch (err) {
        logger.warn('Failed to refresh the Azure AI Foundry server default', err)
      }
      // A logout cleared the reads; the next session's are its own.
      if (epoch !== sessionEpoch) return
      if (read.again) {
        read.again = false
        return once()
      }
      // Settled in the same step as that check, so a change from here on
      // starts a read of its own rather than asking this one for another.
      defaultReads.delete(key)
      if (listing && (loadsApplied.get(key) ?? 0) === loadsBefore) {
        const serverDefault = listing.serverDefault ?? null
        patchScope(key, () => ({serverDefault}))
      }
    }
    void once()
  }

  // Runs a mutation: marks it busy, applies `patch` to the scope on success,
  // relists the chat picker, and types a CONFLICT. `forced`: the request
  // confirmed a re-index (`force`), so the knowledge base — where one exists
  // — asks for one. `movesDefault`: the change may move the server default.
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
        // The user scope lists the organisations' shared entries too.
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
      // The desktop list file turned out to be a newer app version's: the
      // view (registryState) is stale, and the next load shows it read-only.
      if (err instanceof ApiError && err.code === 'REGISTRY_READ_ONLY' && epoch === sessionEpoch) markStale(key)
      throw err
    } finally {
      if (epoch === sessionEpoch) setBusy(key, op, false)
    }
  }

  // The scope's own resource `id` (see ownedHere).
  const findConnection = (scope: AzureScope, s: AzureScopeState, id: string) =>
    s.connections.find(c => c.id === id && ownedHere(scope, c))

  const findModelEntry = (scope: AzureScope, s: AzureScopeState, id: string) =>
    s.models.find(m => m.id === id && ownedHere(scope, m))

  // After a model mutation: sort, recount and re-mark discovery.
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
      // A loaded view stays on screen while it refreshes.
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
            // A mutation landed meanwhile: this view may predate it.
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
            // Drop the discovery of resources that are gone; re-mark the rest.
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
          // Never replace a loaded view with an error screen: keep it and
          // report the failure alongside.
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
          // New settings may reach another project: discover afresh.
          discovery: input.config ? withoutKey(s.discovery, id) : s.discovery,
        }),
        {forced: input.force === true},
      )
      // The models inherit the resource's status (and an agent's support
      // depends on it): relist them when either changed.
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
        // Its models went with it.
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
      // The last result stays on screen while this one loads (and if it fails).
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
        // A designation alone (knowledge base, default for members) leaves it.
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
          // The server recorded it on the saved target; mirror it.
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

/** Selects a scope's state (EMPTY_AZURE_SCOPE before its first load): `useAzureFoundryStore(selectAzureScope(scope))`. */
export function selectAzureScope(scope: AzureScope): (state: AzureFoundryState) => AzureScopeState {
  const key = azureScopeKey(scope)
  return state => state.scopes[key] ?? EMPTY_AZURE_SCOPE
}

// Logout: forget every scope, and let nothing in flight write back.
registerStoreReset(() => {
  sessionEpoch++
  loadsInFlight.clear()
  discoveriesInFlight.clear()
  mutationGen.clear()
  defaultReads.clear()
  loadsApplied.clear()
  useAzureFoundryStore.setState({scopes: {}, knowledgeReindex: {}})
})
