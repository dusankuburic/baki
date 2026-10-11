import {describe, it, expect, vi, beforeEach} from 'vitest'
import type {
  AzureConnection,
  AzureConnectionsListing,
  AzureDiscovery,
  AzureModelEntry,
  AzureModelsListing,
} from '@/types'

vi.mock('@/api', () => ({
  providersApi: {
    listAzureConnections: vi.fn(),
    listAzureModels: vi.fn(),
    createAzureConnection: vi.fn(),
    updateAzureConnection: vi.fn(),
    deleteAzureConnection: vi.fn(),
    discoverAzureConnection: vi.fn(),
    createAzureModel: vi.fn(),
    updateAzureModel: vi.fn(),
    deleteAzureModel: vi.fn(),
    testAzure: vi.fn(),
  },
}))

import {providersApi} from '@/api'
import {ApiError, VersionConflictError} from '@/api/client'
import {resetAllStores} from './storeRegistry'
import {useChatStore} from './chatStore'
import {
  AZURE_DISCOVERY_TTL_MS,
  AZURE_REVALIDATE_MS,
  AzureConflictError,
  EMPTY_AZURE_SCOPE,
  azureBusyKey,
  azureScopeKey,
  selectAzureScope,
  useAzureFoundryStore,
} from './azureFoundryStore'

const api = vi.mocked(providersApi)
const USER = {kind: 'user'} as const
const ORG = {kind: 'org', orgId: 'org-1'} as const

function conn(id: string, over: Partial<AzureConnection> = {}): AzureConnection {
  return {
    id,
    name: `res-${id}`,
    shared: false,
    legacy: id === 'default',
    config: {endpoint: `https://${id}.openai.azure.com`, authMode: 'entra', hasSecret: true},
    status: 'ready',
    canDiscover: true,
    supportsAgents: true,
    suggestDiscovery: false,
    modelCount: 0,
    updatedAt: '2026-09-30T10:00:00Z',
    ...over,
  }
}

function model(id: string, over: Partial<AzureModelEntry> = {}): AzureModelEntry {
  const deployment = over.deployment ?? id
  return {
    id,
    modelId: `m/${id}`,
    connectionId: 'default',
    connectionName: 'res-default',
    kind: 'chat',
    deployment,
    target: deployment,
    displayName: id,
    purpose: 'general',
    isDefault: false,
    isKnowledgeEmbedding: false,
    shared: false,
    status: 'ready',
    updatedAt: '2026-09-30T10:00:00Z',
    ...over,
  }
}

function listing(connections: AzureConnection[], over: Partial<AzureConnectionsListing> = {}): AzureConnectionsListing {
  return {
    connections,
    registryState: 'ok',
    limits: {connections: 10, models: 50},
    knowledgeEmbeddingEnabled: false,
    ...over,
  }
}

function discovery(over: Partial<AzureDiscovery> = {}): AzureDiscovery {
  return {
    deployments: [
      {name: 'gpt-4.1', kind: 'chat', model: 'gpt-4.1', alreadyAdded: false},
      {name: 'text-embedding-3-large', kind: 'embedding', model: 'text-embedding-3-large', alreadyAdded: false},
    ],
    agents: [{name: 'helper', kind: 'agent', model: 'gpt-4o', alreadyAdded: false}],
    warnings: [],
    ...over,
  }
}

function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return {promise, resolve, reject}
}

const scope = (s: typeof USER | typeof ORG = USER) => selectAzureScope(s)(useAzureFoundryStore.getState())
const epoch = () => useChatStore.getState().providerEpoch

async function seed(
  connections: AzureConnection[],
  models: AzureModelEntry[],
  s: typeof USER | typeof ORG = USER,
  over: Partial<AzureConnectionsListing> & {serverDefault?: string} = {},
) {
  const {serverDefault, ...listingOver} = over
  api.listAzureConnections.mockResolvedValueOnce(listing(connections, listingOver))
  api.listAzureModels.mockResolvedValueOnce({models, ...(serverDefault ? {serverDefault} : {})})
  await useAzureFoundryStore.getState().load(s, {force: true})
}

beforeEach(async () => {
  vi.resetAllMocks()
  vi.restoreAllMocks()
  await resetAllStores()
})

describe('azureFoundryStore: loading', () => {
  it('starts every scope empty, with a stable reference for selectors', () => {
    expect(scope()).toBe(EMPTY_AZURE_SCOPE)
    expect(azureScopeKey(USER)).toBe('user')
    expect(azureScopeKey(ORG)).toBe('org:org-1')
  })

  it('loads connections and models, sorted in registry order', async () => {
    const d = deferred<AzureConnectionsListing>()
    api.listAzureConnections.mockReturnValueOnce(d.promise)
    api.listAzureModels.mockResolvedValueOnce({
      models: [
        model('zzreview00', {purpose: 'review', displayName: 'Reviewer'}),
        model('general000', {displayName: 'Everyday'}),
      ],
      serverDefault: 'm/general000',
    })
    const p = useAzureFoundryStore.getState().load(USER)
    expect(scope().status).toBe('loading')

    d.resolve(
      listing([conn('default', {modelCount: 2})], {
        registryState: 'recovered',
        recoveredCopyPath: 'C:\\baki\\azure-models.json.corrupt',
        knowledgeEmbeddingEnabled: true,
      }),
    )
    await p
    const s = scope()
    expect(s.status).toBe('ready')
    expect(s.connections.map(c => c.id)).toEqual(['default'])
    expect(s.models.map(m => m.id)).toEqual(['general000', 'zzreview00'])
    expect(s.registryState).toBe('recovered')
    expect(s.recoveredCopyPath).toBe('C:\\baki\\azure-models.json.corrupt')
    expect(s.knowledgeEmbeddingEnabled).toBe(true)
    expect(s.serverDefault).toBe('m/general000')
    expect(s.loadedAt).toBeGreaterThan(0)

    await seed([conn('default')], [])
    expect(scope()).toMatchObject({serverDefault: null, recoveredCopyPath: null})
  })

  it('dedupes loads while one is in flight', async () => {
    const d = deferred<AzureModelsListing>()
    api.listAzureConnections.mockResolvedValue(listing([]))
    api.listAzureModels.mockReturnValueOnce(d.promise)
    const store = useAzureFoundryStore.getState()
    const a = store.load(USER)
    const b = store.load(USER)
    expect(b).toBe(a)
    d.resolve({models: []})
    await a
    expect(api.listAzureModels).toHaveBeenCalledOnce()
  })

  it('serves a fresh view from the cache and revalidates an old one in the background', async () => {
    const now = vi.spyOn(Date, 'now').mockReturnValue(1_000_000)
    await seed([conn('default')], [model('first00000')])
    await useAzureFoundryStore.getState().load(USER)
    expect(api.listAzureModels).toHaveBeenCalledOnce()

    now.mockReturnValue(1_000_000 + AZURE_REVALIDATE_MS)
    const d = deferred<AzureModelsListing>()
    api.listAzureConnections.mockResolvedValueOnce(listing([conn('default')]))
    api.listAzureModels.mockReturnValueOnce(d.promise)
    const p = useAzureFoundryStore.getState().load(USER)
    expect(scope().status).toBe('ready')
    expect(scope().models.map(m => m.id)).toEqual(['first00000'])
    d.resolve({models: [model('second0000')]})
    await p
    expect(scope().models.map(m => m.id)).toEqual(['second0000'])
  })

  it('refetches a fresh view when forced or invalidated', async () => {
    await seed([conn('default')], [])
    api.listAzureConnections.mockResolvedValue(listing([conn('default')]))
    api.listAzureModels.mockResolvedValue({models: []})
    await useAzureFoundryStore.getState().load(USER, {force: true})
    expect(api.listAzureModels).toHaveBeenCalledTimes(2)

    useAzureFoundryStore.getState().invalidate(USER)
    await useAzureFoundryStore.getState().load(USER)
    expect(api.listAzureModels).toHaveBeenCalledTimes(3)

    await seed([], [], ORG)
    useAzureFoundryStore.getState().invalidate()
    expect(scope(USER).loadedAt).toBe(0)
    expect(scope(ORG).loadedAt).toBe(0)
    useAzureFoundryStore.getState().invalidate({kind: 'org', orgId: 'other'})
    expect(Object.keys(useAzureFoundryStore.getState().scopes).sort()).toEqual(['org:org-1', 'user'])
  })

  it('classifies a failed first load', async () => {
    api.listAzureModels.mockResolvedValue({models: []})
    api.listAzureConnections.mockRejectedValueOnce(new ApiError('boom', 500, 'INTERNAL'))
    await useAzureFoundryStore.getState().load(USER)
    expect(scope()).toMatchObject({status: 'error', errorKind: 'failed', error: 'boom'})

    api.listAzureConnections.mockRejectedValueOnce(new ApiError('admins and members only', 403, 'FORBIDDEN'))
    await useAzureFoundryStore.getState().load(ORG)
    expect(scope(ORG)).toMatchObject({status: 'error', errorKind: 'forbidden'})

    api.listAzureConnections.mockRejectedValueOnce(new ApiError('no org keystore', 404, 'ORG_KEYSTORE_UNAVAILABLE'))
    await useAzureFoundryStore.getState().load(ORG)
    expect(scope(ORG)).toMatchObject({status: 'error', errorKind: 'unavailable'})

    api.listAzureConnections.mockRejectedValueOnce('not an error')
    await useAzureFoundryStore.getState().load(USER)
    expect(scope()).toMatchObject({status: 'error', errorKind: 'failed', error: 'Request failed'})
  })

  it('never replaces a loaded view with an error', async () => {
    await seed([conn('default')], [model('kept000000')])
    api.listAzureConnections.mockRejectedValueOnce(new Error('offline'))
    api.listAzureModels.mockResolvedValueOnce({models: []})
    await useAzureFoundryStore.getState().load(USER, {force: true})
    expect(scope()).toMatchObject({status: 'ready', errorKind: 'failed', error: 'offline'})
    expect(scope().models.map(m => m.id)).toEqual(['kept000000'])

    await seed([conn('default')], [model('kept000000')])
    expect(scope()).toMatchObject({status: 'ready', errorKind: null, error: null})
  })

  it('reads again when a mutation landed while the load was in flight', async () => {
    await seed([conn('default')], [])
    const stale = deferred<AzureModelsListing>()
    api.listAzureConnections.mockResolvedValue(listing([conn('default')]))
    api.listAzureModels.mockReturnValueOnce(stale.promise)
    const p = useAzureFoundryStore.getState().load(USER, {force: true})

    const added = model('added00000')
    api.createAzureModel.mockResolvedValueOnce(added)
    await useAzureFoundryStore
      .getState()
      .createModel(USER, {connectionId: 'default', kind: 'chat', deployment: 'added00000'})
    expect(api.listAzureModels).toHaveBeenCalledTimes(3)
    api.listAzureModels.mockResolvedValueOnce({models: [added]})
    stale.resolve({models: []})
    await p

    expect(api.listAzureModels).toHaveBeenCalledTimes(4)
    expect(scope().models.map(m => m.id)).toEqual(['added00000'])
  })
})

describe('azureFoundryStore: mutations', () => {
  it('adds a resource, marks the work busy, and relists the chat picker', async () => {
    await seed([], [])
    const d = deferred<AzureConnection>()
    api.createAzureConnection.mockReturnValueOnce(d.promise)
    const before = epoch()
    const p = useAzureFoundryStore.getState().createConnection(USER, {
      name: 'contoso',
      config: {endpoint: 'https://contoso.openai.azure.com', auth: {mode: 'api_key', key: 'k'}},
    })
    expect(scope().busy).toEqual({[azureBusyKey.createConnection]: true})
    d.resolve(conn('newconn000'))
    await expect(p).resolves.toMatchObject({id: 'newconn000'})
    expect(scope().connections.map(c => c.id)).toEqual(['newconn000'])
    expect(scope().busy).toEqual({})
    expect(epoch()).toBe(before + 1)
  })

  it('renames a resource on its models too, relisting them only when its status changed', async () => {
    await seed(
      [conn('default')],
      [model('gpt4100000', {connectionName: 'res-default'}), model('other00000', {connectionId: 'x'})],
    )
    api.updateAzureConnection.mockResolvedValueOnce(conn('default', {name: 'renamed'}))
    await useAzureFoundryStore.getState().updateConnection(USER, 'default', {name: 'renamed', updatedAt: 't'})
    expect(scope().connections[0]!.name).toBe('renamed')
    expect(scope().models.find(m => m.id === 'gpt4100000')?.connectionName).toBe('renamed')
    expect(scope().models.find(m => m.id === 'other00000')?.connectionName).toBe('res-default')
    expect(api.listAzureModels).toHaveBeenCalledOnce()

    api.updateAzureConnection.mockResolvedValueOnce(conn('default', {name: 'renamed', status: 'needs_credentials'}))
    api.listAzureConnections.mockResolvedValueOnce(listing([conn('default', {status: 'needs_credentials'})]))
    api.listAzureModels.mockResolvedValueOnce({models: [model('gpt4100000', {status: 'needs_credentials'})]})
    await useAzureFoundryStore.getState().updateConnection(USER, 'default', {dismissDiscovery: true})
    await vi.waitFor(() => expect(scope().models[0]!.status).toBe('needs_credentials'))
    expect(api.listAzureModels).toHaveBeenCalledTimes(2)
  })

  it('drops cached discovery when a resource gets new settings', async () => {
    await seed([conn('default')], [])
    api.discoverAzureConnection.mockResolvedValueOnce(discovery())
    await useAzureFoundryStore.getState().discover(USER, 'default')
    api.updateAzureConnection.mockResolvedValueOnce(conn('default'))
    await useAzureFoundryStore.getState().updateConnection(USER, 'default', {name: 'same-status'})
    expect(scope().discovery.default).toBeDefined()

    api.updateAzureConnection.mockResolvedValueOnce(conn('default'))
    await useAzureFoundryStore.getState().updateConnection(USER, 'default', {
      config: {endpoint: 'https://elsewhere.openai.azure.com', auth: {mode: 'entra', tenantId: 't', clientId: 'c'}},
    })
    expect(scope().discovery.default).toBeUndefined()
  })

  it('dismisses discovery with a PUT', async () => {
    await seed([conn('default', {suggestDiscovery: true})], [])
    api.updateAzureConnection.mockResolvedValueOnce(conn('default', {suggestDiscovery: false}))
    await useAzureFoundryStore.getState().dismissDiscovery(USER, 'default')
    expect(api.updateAzureConnection).toHaveBeenCalledWith(USER, 'default', {dismissDiscovery: true})
    expect(scope().connections[0]!.suggestDiscovery).toBe(false)
  })

  it("removes a resource with its models and discovery, leaving an org's resource of the same id alone", async () => {
    const orgDefault = conn('default', {shared: true, orgId: 'org-1', orgName: 'Contoso'})
    const orgModel = model('orgmodel00', {shared: true, orgId: 'org-1'})
    await seed(
      [conn('default'), orgDefault, conn('keep000000')],
      [model('mine000000'), orgModel, model('kept000000', {connectionId: 'keep000000'})],
    )
    api.discoverAzureConnection.mockResolvedValueOnce(discovery())
    await useAzureFoundryStore.getState().discover(USER, 'default')

    api.deleteAzureConnection.mockResolvedValueOnce({status: 'ok', deletedModels: 1, clearedKnowledgeEmbedding: false})
    await expect(
      useAzureFoundryStore.getState().deleteConnection(USER, 'default', {cascade: true}),
    ).resolves.toMatchObject({deletedModels: 1})
    expect(api.deleteAzureConnection).toHaveBeenCalledWith(USER, 'default', {cascade: true})
    expect(scope().connections).toEqual([orgDefault, conn('keep000000')])
    expect(scope().models.map(m => m.id)).toEqual(['kept000000', 'orgmodel00'])
    expect(scope().discovery).toEqual({})
  })

  it('adds a model: sorted in, counted on its resource, marked in discovery', async () => {
    await seed([conn('default', {modelCount: 1})], [model('zzz0000000', {displayName: 'Zed'})])
    api.discoverAzureConnection.mockResolvedValueOnce(discovery())
    await useAzureFoundryStore.getState().discover(USER, 'default')

    const added = model('gpt4100000', {deployment: 'GPT-4.1', displayName: 'Alpha'})
    api.createAzureModel.mockResolvedValueOnce(added)
    const before = epoch()
    await useAzureFoundryStore
      .getState()
      .createModel(USER, {connectionId: 'default', kind: 'chat', deployment: 'GPT-4.1'})
    const s = scope()
    expect(s.models.map(m => m.id)).toEqual(['gpt4100000', 'zzz0000000'])
    expect(s.connections[0]!.modelCount).toBe(2)
    expect(s.discovery.default!.data!.deployments[0]!).toEqual({
      name: 'gpt-4.1',
      kind: 'chat',
      model: 'gpt-4.1',
      alreadyAdded: true,
      modelId: 'm/gpt4100000',
    })
    expect(epoch()).toBe(before + 1)
  })

  it('keeps one default and one knowledge-base model per owner', async () => {
    const orgA = model('orga000000', {shared: true, isDefault: true})
    const orgB = model('orgb000000', {shared: true})
    const kbOld = model('kbold00000', {kind: 'embedding', isKnowledgeEmbedding: true, shared: true})
    const kbNew = model('kbnew00000', {kind: 'embedding', shared: true})
    await seed([conn('default', {shared: true})], [orgA, orgB, kbOld, kbNew], ORG)

    api.updateAzureModel.mockResolvedValueOnce({...orgB, isDefault: true})
    await useAzureFoundryStore.getState().updateModel(ORG, 'orgb000000', {isDefault: true})
    api.updateAzureModel.mockResolvedValueOnce({...kbNew, isKnowledgeEmbedding: true})
    await useAzureFoundryStore.getState().updateModel(ORG, 'kbnew00000', {isKnowledgeEmbedding: true, force: true})

    const byId = Object.fromEntries(scope(ORG).models.map(m => [m.id, m]))
    expect(byId.orga000000!.isDefault).toBe(false)
    expect(byId.orgb000000!.isDefault).toBe(true)
    expect(byId.kbold00000!.isKnowledgeEmbedding).toBe(false)
    expect(byId.kbnew00000!.isKnowledgeEmbedding).toBe(true)
  })

  it('asks the knowledge base for a re-index after a change confirmed with force, per scope', async () => {
    const kb = model('kbnew00000', {kind: 'embedding'})
    await seed([conn('default')], [kb, model('gpt4100000')], USER, {knowledgeEmbeddingEnabled: true})
    await seed([conn('default', {shared: true})], [{...kb, shared: true}], ORG, {knowledgeEmbeddingEnabled: true})
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({})

    api.updateAzureModel.mockResolvedValueOnce({...kb, isKnowledgeEmbedding: true})
    await useAzureFoundryStore.getState().updateModel(USER, 'kbnew00000', {isKnowledgeEmbedding: true})
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({})

    api.updateAzureModel.mockResolvedValueOnce({...kb, shared: true, isKnowledgeEmbedding: true})
    await useAzureFoundryStore.getState().updateModel(ORG, 'kbnew00000', {isKnowledgeEmbedding: true, force: true})
    api.deleteAzureModel.mockResolvedValueOnce({status: 'ok'})
    await useAzureFoundryStore.getState().deleteModel(USER, 'kbnew00000', {force: true})
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({user: true, 'org:org-1': true})

    useAzureFoundryStore.getState().clearKnowledgeReindex([USER])
    api.updateAzureConnection.mockRejectedValueOnce(new ApiError('nope', 500, 'INTERNAL'))
    await expect(
      useAzureFoundryStore.getState().updateConnection(USER, 'default', {force: true, name: 'x'}),
    ).rejects.toThrow('nope')
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({'org:org-1': true})

    api.updateAzureConnection.mockResolvedValueOnce(conn('default'))
    await useAzureFoundryStore.getState().updateConnection(USER, 'default', {force: true, name: 'x'})
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({user: true, 'org:org-1': true})
    useAzureFoundryStore.getState().clearKnowledgeReindex()
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({})
    api.deleteAzureConnection.mockResolvedValueOnce({status: 'ok', deletedModels: 1, clearedKnowledgeEmbedding: true})
    await useAzureFoundryStore.getState().deleteConnection(ORG, 'default', {cascade: true, force: true})
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({'org:org-1': true})

    const before = useAzureFoundryStore.getState().knowledgeReindex
    useAzureFoundryStore.getState().clearKnowledgeReindex([USER])
    expect(useAzureFoundryStore.getState().knowledgeReindex).toBe(before)

    await resetAllStores()
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({})
  })

  it('asks for no re-index where no Azure knowledge base exists', async () => {
    const kb = model('kbnew00000', {kind: 'embedding', isKnowledgeEmbedding: true})
    await seed([conn('default')], [kb], USER, {knowledgeEmbeddingEnabled: false})
    api.deleteAzureModel.mockResolvedValueOnce({status: 'ok'})
    await useAzureFoundryStore.getState().deleteModel(USER, 'kbnew00000', {force: true})
    api.deleteAzureConnection.mockResolvedValueOnce({status: 'ok', deletedModels: 0, clearedKnowledgeEmbedding: false})
    await useAzureFoundryStore.getState().deleteConnection(USER, 'default', {force: true})
    expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({})
  })

  it("never touches another owner's designation", async () => {
    const mine = model('mine000000', {kind: 'embedding', isKnowledgeEmbedding: true})
    const orgs = model('orgkb00000', {kind: 'embedding', isKnowledgeEmbedding: true, shared: true, orgId: 'org-1'})
    const other = model('other00000', {kind: 'embedding'})
    await seed([conn('default')], [mine, orgs, other])
    api.updateAzureModel.mockResolvedValueOnce({...other, isKnowledgeEmbedding: true})
    await useAzureFoundryStore.getState().updateModel(USER, 'other00000', {isKnowledgeEmbedding: true})
    const byId = Object.fromEntries(scope().models.map(m => [m.id, m]))
    expect(byId.mine000000!.isKnowledgeEmbedding).toBe(false)
    expect(byId.orgkb00000!.isKnowledgeEmbedding).toBe(true)
  })

  it('re-marks only the discovery a change affects, and skips one still without data', async () => {
    await seed([conn('default'), conn('second0000'), conn('third00000')], [])
    api.discoverAzureConnection.mockResolvedValueOnce(discovery())
    await useAzureFoundryStore.getState().discover(USER, 'default')
    api.discoverAzureConnection.mockRejectedValueOnce(new Error('offline'))
    await useAzureFoundryStore
      .getState()
      .discover(USER, 'second0000')
      .catch(() => {})
    const cached = scope().discovery

    api.createAzureModel.mockResolvedValueOnce(model('third-gpt0', {connectionId: 'third00000', deployment: 'gpt-4.1'}))
    await useAzureFoundryStore
      .getState()
      .createModel(USER, {connectionId: 'third00000', kind: 'chat', deployment: 'gpt-4.1'})
    expect(scope().discovery.default).toBe(cached.default)
    expect(scope().discovery.second0000).toBe(cached.second0000)

    api.deleteAzureConnection.mockResolvedValueOnce({status: 'ok', deletedModels: 1, clearedKnowledgeEmbedding: false})
    await useAzureFoundryStore.getState().deleteConnection(USER, 'third00000', {cascade: true})
    expect(scope().discovery).toBe(cached)

    await seed([conn('second0000'), conn('default')], [])
    expect(Object.keys(scope().discovery).sort()).toEqual(['default', 'second0000'])
  })

  it('moves a model between resources: counts and discovery follow', async () => {
    await seed([conn('default', {modelCount: 1}), conn('second0000')], [model('gpt4100000', {deployment: 'gpt-4.1'})])
    api.discoverAzureConnection.mockResolvedValueOnce(
      discovery({
        deployments: [{name: 'gpt-4.1', kind: 'chat', model: 'gpt-4.1', alreadyAdded: true, modelId: 'm/gpt4100000'}],
      }),
    )
    await useAzureFoundryStore.getState().discover(USER, 'default')

    api.updateAzureModel.mockResolvedValueOnce(model('gpt4100000', {deployment: 'gpt-4.1', connectionId: 'second0000'}))
    await useAzureFoundryStore.getState().updateModel(USER, 'gpt4100000', {connectionId: 'second0000'})
    const s = scope()
    expect(s.connections.map(c => [c.id, c.modelCount])).toEqual([
      ['default', 0],
      ['second0000', 1],
    ])
    expect(s.discovery.default!.data!.deployments[0]!).toEqual({
      name: 'gpt-4.1',
      kind: 'chat',
      model: 'gpt-4.1',
      alreadyAdded: false,
    })
  })

  it('removes a model: uncounted and unmarked in discovery', async () => {
    await seed([conn('default', {modelCount: 1})], [model('helper0000', {kind: 'agent', deployment: 'helper'})])
    api.discoverAzureConnection.mockResolvedValueOnce(discovery())
    const d = await useAzureFoundryStore.getState().discover(USER, 'default')
    expect(d.agents[0]).toMatchObject({alreadyAdded: true, modelId: 'm/helper0000'})

    api.deleteAzureModel.mockResolvedValueOnce({status: 'ok'})
    await useAzureFoundryStore.getState().deleteModel(USER, 'helper0000', {force: true})
    expect(api.deleteAzureModel).toHaveBeenCalledWith(USER, 'helper0000', {force: true})
    expect(scope().models).toEqual([])
    expect(scope().connections[0]!.modelCount).toBe(0)
    expect(scope().discovery.default!.data!.agents[0]!).toEqual({
      name: 'helper',
      kind: 'agent',
      model: 'gpt-4o',
      alreadyAdded: false,
    })

    api.deleteAzureModel.mockResolvedValueOnce({status: 'ok'})
    await useAzureFoundryStore.getState().deleteModel(USER, 'unknown000')
    expect(scope().connections[0]!.modelCount).toBe(0)
  })

  it('marks the user scope stale after an org change (it lists the shared entries)', async () => {
    await seed([], [])
    await seed([conn('default', {shared: true, orgId: 'org-1'})], [], ORG)
    api.createAzureModel.mockResolvedValueOnce(model('shared0000', {shared: true, orgId: 'org-1'}))
    await useAzureFoundryStore.getState().createModel(ORG, {connectionId: 'default', kind: 'chat', deployment: 'x'})
    expect(scope(USER).loadedAt).toBe(0)
    expect(scope(ORG).loadedAt).toBeGreaterThan(0)
  })

  it('rejects a CONFLICT with AzureConflictError and marks the scope stale', async () => {
    await seed([conn('default')], [])
    const before = epoch()
    api.updateAzureConnection.mockRejectedValueOnce(
      new VersionConflictError('changed by another admin', 'CONFLICT', 'req-1', {current: 't2'}),
    )
    const err = await useAzureFoundryStore
      .getState()
      .updateConnection(ORG, 'default', {name: 'x'})
      .catch(e => e)
    expect(err).toBeInstanceOf(AzureConflictError)
    expect(err).toBeInstanceOf(VersionConflictError)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({status: 409, code: 'CONFLICT', requestId: 'req-1', details: {current: 't2'}})
    expect(epoch()).toBe(before)

    api.updateAzureConnection.mockRejectedValueOnce(new VersionConflictError('changed', 'CONFLICT'))
    await useAzureFoundryStore
      .getState()
      .updateConnection(USER, 'default', {name: 'x'})
      .catch(() => {})
    expect(scope().loadedAt).toBe(0)
    expect(scope().busy).toEqual({})
  })

  it('passes every other failure through as is', async () => {
    await seed([conn('default')], [])
    const dup = new VersionConflictError('already added', 'DUPLICATE_MODEL', null, {existingModelId: 'm/aaaaaaaaaa'})
    api.createAzureModel.mockRejectedValueOnce(dup)
    await expect(
      useAzureFoundryStore.getState().createModel(USER, {connectionId: 'default', kind: 'chat', deployment: 'gpt-4.1'}),
    ).rejects.toBe(dup)
    expect(scope().busy).toEqual({})
    expect(scope().loadedAt).toBeGreaterThan(0)
  })

  it('marks the scope stale when the desktop list file turns out to be read-only', async () => {
    await seed([conn('default')], [model('gpt4100000')])
    const readOnly = new VersionConflictError('written by a newer version', 'REGISTRY_READ_ONLY')
    api.deleteAzureModel.mockRejectedValueOnce(readOnly)
    await expect(useAzureFoundryStore.getState().deleteModel(USER, 'gpt4100000')).rejects.toBe(readOnly)
    expect(scope().loadedAt).toBe(0)
    expect(scope().models.map(m => m.id)).toEqual(['gpt4100000'])
  })
})

describe('azureFoundryStore: the server default', () => {
  const reads = () => api.listAzureModels.mock.calls.length

  it('is read again once the model it named is removed — and only it is taken from that read', async () => {
    await seed([conn('default')], [model('aaaaaaaaaa'), model('bbbbbbbbbb')], USER, {serverDefault: 'm/aaaaaaaaaa'})
    api.deleteAzureModel.mockResolvedValueOnce({status: 'ok'})
    api.listAzureModels.mockResolvedValueOnce({
      models: [model('bbbbbbbbbb'), model('notmine000')],
      serverDefault: 'm/bbbbbbbbbb',
    })
    await useAzureFoundryStore.getState().deleteModel(USER, 'aaaaaaaaaa')
    await vi.waitFor(() => expect(scope().serverDefault).toBe('m/bbbbbbbbbb'))
    expect(reads()).toBe(2)
    expect(scope().models.map(m => m.id)).toEqual(['bbbbbbbbbb'])
  })

  it('is read after an add, a change of kind and a resource removal — not after a designation, nor for an org', async () => {
    await seed([conn('default')], [model('aaaaaaaaaa'), model('embed00000', {kind: 'embedding'})], USER, {
      serverDefault: 'm/aaaaaaaaaa',
    })
    let n = reads()

    api.createAzureModel.mockResolvedValueOnce(model('aaanew0000', {displayName: 'aaa first'}))
    api.listAzureModels.mockResolvedValueOnce({models: [], serverDefault: 'm/aaanew0000'})
    await useAzureFoundryStore
      .getState()
      .createModel(USER, {connectionId: 'default', kind: 'chat', deployment: 'aaanew0000'})
    await vi.waitFor(() => expect(scope().serverDefault).toBe('m/aaanew0000'))
    expect(reads()).toBe(++n)

    api.updateAzureModel.mockResolvedValueOnce(model('aaanew0000', {kind: 'embedding'}))
    api.listAzureModels.mockResolvedValueOnce({models: [], serverDefault: 'm/aaaaaaaaaa'})
    await useAzureFoundryStore.getState().updateModel(USER, 'aaanew0000', {kind: 'embedding'})
    await vi.waitFor(() => expect(scope().serverDefault).toBe('m/aaaaaaaaaa'))
    expect(reads()).toBe(++n)

    api.updateAzureModel.mockResolvedValueOnce(model('embed00000', {kind: 'embedding', isKnowledgeEmbedding: true}))
    await useAzureFoundryStore.getState().updateModel(USER, 'embed00000', {isKnowledgeEmbedding: true})
    expect(reads()).toBe(n)

    api.deleteAzureConnection.mockResolvedValueOnce({status: 'ok', deletedModels: 3, clearedKnowledgeEmbedding: true})
    api.listAzureModels.mockResolvedValueOnce({models: []})
    await useAzureFoundryStore.getState().deleteConnection(USER, 'default', {cascade: true})
    await vi.waitFor(() => expect(scope().serverDefault).toBeNull())
    expect(reads()).toBe(n + 1)

    await seed([conn('default', {shared: true})], [], ORG)
    const seeded = reads()
    api.createAzureModel.mockResolvedValueOnce(model('orgmodel00', {shared: true}))
    await useAzureFoundryStore.getState().createModel(ORG, {connectionId: 'default', kind: 'chat', deployment: 'x'})
    expect(reads()).toBe(seeded)
  })

  it('reads once more when another change lands while a read is out, dropping the stale answer', async () => {
    const [a, b, c] = [model('aaaaaaaaaa'), model('bbbbbbbbbb'), model('cccccccccc')]
    await seed([conn('default')], [a, b, c], USER, {serverDefault: 'm/aaaaaaaaaa'})
    const first = deferred<AzureModelsListing>()
    api.listAzureModels.mockReturnValueOnce(first.promise)
    api.deleteAzureModel.mockResolvedValue({status: 'ok'})
    await useAzureFoundryStore.getState().deleteModel(USER, 'aaaaaaaaaa')
    await useAzureFoundryStore.getState().deleteModel(USER, 'bbbbbbbbbb')
    expect(reads()).toBe(2)

    api.listAzureModels.mockResolvedValueOnce({models: [c], serverDefault: 'm/cccccccccc'})
    first.resolve({models: [b, c], serverDefault: 'm/bbbbbbbbbb'})
    await vi.waitFor(() => expect(reads()).toBe(3))
    await vi.waitFor(() => expect(scope().serverDefault).toBe('m/cccccccccc'))
  })

  it('drops its answer when a load lands meanwhile, which is at least as fresh', async () => {
    const [a, b, c] = [model('aaaaaaaaaa'), model('bbbbbbbbbb'), model('cccccccccc')]
    await seed([conn('default')], [a, b, c], USER, {serverDefault: 'm/aaaaaaaaaa'})
    const late = deferred<AzureModelsListing>()
    api.listAzureModels.mockReturnValueOnce(late.promise)
    api.deleteAzureModel.mockResolvedValueOnce({status: 'ok'})
    await useAzureFoundryStore.getState().deleteModel(USER, 'aaaaaaaaaa')

    await seed([conn('default')], [b, c], USER, {serverDefault: 'm/cccccccccc'})
    expect(scope().serverDefault).toBe('m/cccccccccc')
    late.resolve({models: [b, c], serverDefault: 'm/bbbbbbbbbb'})
    await new Promise(resolve => setTimeout(resolve, 0))
    expect(scope().serverDefault).toBe('m/cccccccccc')
  })

  it('keeps the last value when the read fails, and reads again on the next change', async () => {
    await seed([conn('default')], [model('aaaaaaaaaa')], USER, {serverDefault: 'm/aaaaaaaaaa'})
    api.createAzureModel.mockResolvedValueOnce(model('bbbbbbbbbb'))
    api.listAzureModels.mockRejectedValueOnce(new Error('offline'))
    await useAzureFoundryStore.getState().createModel(USER, {connectionId: 'default', kind: 'chat', deployment: 'b'})
    await vi.waitFor(() => expect(reads()).toBe(2))
    await Promise.resolve()
    expect(scope().serverDefault).toBe('m/aaaaaaaaaa')

    api.deleteAzureModel.mockResolvedValueOnce({status: 'ok'})
    api.listAzureModels.mockResolvedValueOnce({models: [], serverDefault: 'm/bbbbbbbbbb'})
    await useAzureFoundryStore.getState().deleteModel(USER, 'aaaaaaaaaa')
    await vi.waitFor(() => expect(scope().serverDefault).toBe('m/bbbbbbbbbb'))
  })

  it('writes nothing back after a logout', async () => {
    await seed([conn('default')], [model('aaaaaaaaaa')], USER, {serverDefault: 'm/aaaaaaaaaa'})
    const late = deferred<AzureModelsListing>()
    api.listAzureModels.mockReturnValueOnce(late.promise)
    api.deleteAzureModel.mockResolvedValueOnce({status: 'ok'})
    await useAzureFoundryStore.getState().deleteModel(USER, 'aaaaaaaaaa')
    await resetAllStores()
    late.resolve({models: [], serverDefault: 'm/stale00000'})
    await Promise.resolve()
    expect(useAzureFoundryStore.getState().scopes).toEqual({})

    await seed([conn('default')], [model('bbbbbbbbbb')], USER, {serverDefault: 'm/bbbbbbbbbb'})
    api.deleteAzureModel.mockResolvedValueOnce({status: 'ok'})
    api.listAzureModels.mockResolvedValueOnce({models: [], serverDefault: 'm/cccccccccc'})
    await useAzureFoundryStore.getState().deleteModel(USER, 'bbbbbbbbbb')
    await vi.waitFor(() => expect(scope().serverDefault).toBe('m/cccccccccc'))
  })
})

describe('azureFoundryStore: discovery', () => {
  it('caches per resource for five minutes, and refreshes when forced', async () => {
    const now = vi.spyOn(Date, 'now').mockReturnValue(5_000_000)
    await seed([conn('default')], [])
    api.discoverAzureConnection.mockResolvedValue(discovery())
    const store = useAzureFoundryStore.getState()
    await store.discover(USER, 'default')
    await store.discover(USER, 'default')
    expect(api.discoverAzureConnection).toHaveBeenCalledOnce()

    await store.discover(USER, 'default', {force: true})
    expect(api.discoverAzureConnection).toHaveBeenCalledTimes(2)

    now.mockReturnValue(5_000_000 + AZURE_DISCOVERY_TTL_MS)
    await store.discover(USER, 'default')
    expect(api.discoverAzureConnection).toHaveBeenCalledTimes(3)
  })

  it('dedupes a discovery in flight and marks it busy', async () => {
    await seed([conn('default')], [])
    const d = deferred<AzureDiscovery>()
    api.discoverAzureConnection.mockReturnValueOnce(d.promise)
    const store = useAzureFoundryStore.getState()
    const a = store.discover(USER, 'default')
    const b = store.discover(USER, 'default')
    expect(b).toBe(a)
    expect(scope().discovery.default!.status).toBe('loading')
    expect(scope().busy).toEqual({[azureBusyKey.discover('default')]: true})
    d.resolve(discovery())
    await a
    expect(scope().discovery.default!.status).toBe('ready')
    expect(scope().busy).toEqual({})
  })

  it('keeps the last result on a failure and rejects', async () => {
    await seed([conn('default')], [])
    api.discoverAzureConnection.mockResolvedValueOnce(discovery())
    await useAzureFoundryStore.getState().discover(USER, 'default')
    api.discoverAzureConnection.mockRejectedValueOnce(new ApiError('needs Entra', 400, 'DISCOVERY_UNAVAILABLE'))
    await expect(useAzureFoundryStore.getState().discover(USER, 'default', {force: true})).rejects.toMatchObject({
      code: 'DISCOVERY_UNAVAILABLE',
    })
    expect(scope().discovery.default).toMatchObject({
      status: 'error',
      error: 'needs Entra',
      errorCode: 'DISCOVERY_UNAVAILABLE',
    })
    expect(scope().discovery.default!.data?.deployments).toHaveLength(2)

    api.discoverAzureConnection.mockRejectedValueOnce(new Error('offline'))
    await useAzureFoundryStore
      .getState()
      .discover(USER, 'second0000')
      .catch(() => {})
    expect(scope().discovery.second0000).toMatchObject({status: 'error', data: null, errorCode: null})
  })

  it('does not reuse an Azure refusal', async () => {
    await seed([conn('default')], [])
    api.discoverAzureConnection.mockResolvedValueOnce(discovery({error: 'missing role'}))
    api.discoverAzureConnection.mockResolvedValueOnce(discovery())
    await useAzureFoundryStore.getState().discover(USER, 'default')
    await useAzureFoundryStore.getState().discover(USER, 'default')
    expect(api.discoverAzureConnection).toHaveBeenCalledTimes(2)
  })

  it("marks a deployment added by an entry of any non-agent kind, and an agent only by an agent entry — the server's rule", async () => {
    await seed(
      [conn('default')],
      [
        model('claude0000', {kind: 'claude', deployment: 'GPT-4.1'}),
        model('embchat000', {kind: 'chat', deployment: 'text-embedding-3-large'}),
        model('helperchat', {kind: 'chat', deployment: 'helper'}),
      ],
    )
    api.discoverAzureConnection.mockResolvedValueOnce(discovery())
    const d = await useAzureFoundryStore.getState().discover(USER, 'default')
    expect(d.deployments.map(i => [i.name, i.alreadyAdded, i.modelId])).toEqual([
      ['gpt-4.1', true, 'm/claude0000'],
      ['text-embedding-3-large', true, 'm/embchat000'],
    ])
    expect(d.agents[0]).toMatchObject({name: 'helper', alreadyAdded: false})
    expect(d.agents[0]).not.toHaveProperty('modelId')
  })

  it('forgets the discovery of a resource that is gone on the next load', async () => {
    await seed([conn('default')], [])
    api.discoverAzureConnection.mockResolvedValueOnce(discovery())
    await useAzureFoundryStore.getState().discover(USER, 'default')
    await seed([], [])
    expect(scope().discovery).toEqual({})
  })
})

describe('azureFoundryStore: tests', () => {
  it("mirrors a saved model's or resource's result, never a draft's", async () => {
    vi.spyOn(Date.prototype, 'toISOString').mockReturnValue('2026-09-30T12:00:00.000Z')
    const orgTwin = conn('default', {shared: true, orgId: 'org-1'})
    await seed([conn('default'), orgTwin], [model('gpt4100000'), model('zzother000')])
    api.testAzure.mockResolvedValueOnce({ok: true, latencyMs: 120})
    await useAzureFoundryStore.getState().test(USER, {model: {id: 'gpt4100000'}})
    expect(scope().models[0]!.lastTest).toEqual({ok: true, latencyMs: 120, at: '2026-09-30T12:00:00.000Z'})
    expect(scope().models[1]!.lastTest).toBeUndefined()

    api.testAzure.mockResolvedValueOnce({ok: false, latencyMs: 30, error: 'bad key', code: 'PROVIDER_AUTH'})
    await expect(useAzureFoundryStore.getState().test(USER, {connectionId: 'default'})).resolves.toMatchObject({
      ok: false,
    })
    expect(scope().connections[0]!.lastTest).toEqual({
      ok: false,
      latencyMs: 30,
      error: 'bad key',
      at: '2026-09-30T12:00:00.000Z',
    })
    expect(scope().connections[1]).toBe(orgTwin)

    const before = scope()
    api.testAzure.mockResolvedValueOnce({ok: true, latencyMs: 5})
    await useAzureFoundryStore
      .getState()
      .test(USER, {connectionId: 'default', model: {kind: 'chat', deployment: 'draft'}})
    api.testAzure.mockResolvedValueOnce({ok: true, latencyMs: 5})
    await useAzureFoundryStore
      .getState()
      .test(USER, {config: {endpoint: 'https://x.openai.azure.com', auth: {mode: 'api_key'}}})
    api.testAzure.mockResolvedValueOnce({ok: true, latencyMs: 5})
    await useAzureFoundryStore.getState().test(USER, {})
    expect(scope().models).toBe(before.models)
    expect(scope().connections).toBe(before.connections)

    api.testAzure.mockResolvedValueOnce({ok: true, latencyMs: 5})
    await useAzureFoundryStore.getState().test(USER, {model: {id: 'unknown000'}})
    api.testAzure.mockResolvedValueOnce({ok: true, latencyMs: 5})
    await useAzureFoundryStore.getState().test(USER, {connectionId: 'unknown000'})
    expect(scope().models).toEqual(before.models)
    expect(scope().connections).toEqual(before.connections)
  })

  it('marks a test busy by its target and clears it on failure', async () => {
    await seed([conn('default')], [])
    const d = deferred<never>()
    api.testAzure.mockReturnValueOnce(d.promise)
    const p = useAzureFoundryStore.getState().test(USER, {connectionId: 'default'})
    expect(scope().busy).toEqual({'test:connection:default': true})
    d.reject(new ApiError('not found', 404, 'CONNECTION_NOT_FOUND'))
    await expect(p).rejects.toMatchObject({code: 'CONNECTION_NOT_FOUND'})
    expect(scope().busy).toEqual({})

    const draft = {endpoint: 'e', auth: {mode: 'api_key' as const}}
    expect(azureBusyKey.test({model: {id: 'x'}})).toBe('test:model:x')
    expect(azureBusyKey.test({connectionId: 'c', model: {id: 'x'}})).toBe('test:model:x')
    expect(azureBusyKey.test({connectionId: 'c', model: {kind: 'agent', deployment: 'helper'}})).toBe('test:draft')
    expect(azureBusyKey.test({config: draft})).toBe('test:draft')
    expect(azureBusyKey.test({connectionId: 'default', config: draft})).toBe('test:draft')
    expect(azureBusyKey.test({})).toBe('test:draft')
  })
})

describe('azureFoundryStore: logout', () => {
  it('forgets every scope and lets nothing in flight write back', async () => {
    await seed([conn('default')], [model('gpt4100000')])
    const late = deferred<AzureModelsListing>()
    api.listAzureConnections.mockResolvedValueOnce(listing([conn('default')]))
    api.listAzureModels.mockReturnValueOnce(late.promise)
    const loading = useAzureFoundryStore.getState().load(ORG)
    const failing = deferred<AzureConnectionsListing>()
    api.listAzureConnections.mockReturnValueOnce(failing.promise)
    api.listAzureModels.mockResolvedValueOnce({models: []})
    const failingLoad = useAzureFoundryStore.getState().load({kind: 'org', orgId: 'org-2'})

    const created = deferred<AzureModelEntry>()
    api.createAzureModel.mockReturnValueOnce(created.promise)
    const creating = useAzureFoundryStore
      .getState()
      .createModel(USER, {connectionId: 'default', kind: 'chat', deployment: 'x'})
    const conflict = deferred<AzureConnection>()
    api.updateAzureConnection.mockReturnValueOnce(conflict.promise)
    const updating = useAzureFoundryStore.getState().updateConnection(USER, 'default', {name: 'x'})
    const found = deferred<AzureDiscovery>()
    api.discoverAzureConnection.mockReturnValueOnce(found.promise)
    const discovering = useAzureFoundryStore.getState().discover(USER, 'default')
    const refused = deferred<AzureDiscovery>()
    api.discoverAzureConnection.mockReturnValueOnce(refused.promise)
    const refusing = useAzureFoundryStore.getState().discover(USER, 'second0000')
    const tested = deferred<{ok: boolean; latencyMs: number}>()
    api.testAzure.mockReturnValueOnce(tested.promise)
    const testing = useAzureFoundryStore.getState().test(USER, {connectionId: 'default'})
    const before = epoch()

    await resetAllStores()
    expect(useAzureFoundryStore.getState().scopes).toEqual({})

    late.resolve({models: [model('stale00000')]})
    failing.reject(new Error('gone'))
    created.resolve(model('late000000'))
    conflict.reject(new VersionConflictError('changed', 'CONFLICT'))
    found.resolve(discovery())
    refused.reject(new Error('offline'))
    tested.resolve({ok: true, latencyMs: 1})
    await loading
    await failingLoad
    await creating
    await expect(updating).rejects.toBeInstanceOf(AzureConflictError)
    await discovering
    await expect(refusing).rejects.toThrow('offline')
    await testing
    expect(useAzureFoundryStore.getState().scopes).toEqual({})
    expect(epoch()).toBeLessThanOrEqual(before)

    api.discoverAzureConnection.mockResolvedValueOnce(discovery())
    await useAzureFoundryStore.getState().discover(USER, 'default')
    expect(api.discoverAzureConnection).toHaveBeenCalledTimes(3)
  })
})
