import {describe, it, expect, vi, afterEach} from 'vitest'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({
      apiUrl: 'http://localhost:9999',
      token: 'mock-token',
      port: 9999,
    }),
  }),
}))

function respond(payload: unknown, status = 200) {
  return vi.spyOn(globalThis, 'fetch').mockImplementation(
    async () =>
      new Response(JSON.stringify(payload), {
        status,
        headers: {'Content-Type': 'application/json'},
      }),
  )
}

type Call = [string, RequestInit]

function call(spy: ReturnType<typeof respond>, i = 0) {
  const [url, init] = spy.mock.calls[i] as Call
  return {
    path: url.replace('http://localhost:9999', ''),
    method: init.method,
    body: init.body === undefined ? undefined : JSON.parse(init.body as string),
  }
}

async function load() {
  const mod = await import('./providers')
  const client = await import('./client')
  return {api: mod.providersApi, azurePath: mod.azurePath, client}
}

const USER = {kind: 'user'} as const
const ORG = {kind: 'org', orgId: 'org 1'} as const

const aConn = (over: Record<string, unknown> = {}) => ({
  id: 'default',
  name: 'my-res',
  shared: false,
  legacy: false,
  config: {endpoint: 'https://contoso.openai.azure.com', authMode: 'api_key', hasSecret: true},
  status: 'ready',
  canDiscover: false,
  supportsAgents: false,
  suggestDiscovery: false,
  modelCount: 0,
  lastTest: null,
  updatedAt: '2026-01-01T00:00:00Z',
  ...over,
})

const aModel = (over: Record<string, unknown> = {}) => ({
  id: 'bbbbbbbbbb',
  modelId: 'm/bbbbbbbbbb',
  connectionId: 'default',
  connectionName: 'my-res',
  kind: 'chat',
  deployment: 'gpt-4.1',
  target: 'gpt-4.1',
  displayName: 'GPT-4.1',
  purpose: 'general',
  isDefault: false,
  isKnowledgeEmbedding: false,
  shared: false,
  status: 'ready',
  lastTest: null,
  updatedAt: '2026-01-01T00:00:00Z',
  ...over,
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.resetModules()
})

describe('azurePath', () => {
  it('builds the personal and the org routes, encoding every segment', async () => {
    const {azurePath} = await load()
    expect(azurePath(USER)).toBe('/api/providers/azure-foundry')
    expect(azurePath(USER, 'connections')).toBe('/api/providers/azure-foundry/connections')
    expect(azurePath(ORG, 'models', 'a/b')).toBe('/api/orgs/org%201/providers/azure-foundry/models/a%2Fb')
    expect(azurePath(ORG, 'connections', 'default', 'discover')).toBe(
      '/api/orgs/org%201/providers/azure-foundry/connections/default/discover',
    )
  })
})

describe('providersApi Azure registry: connections', () => {
  it('lists with a GET that bypasses the micro-cache and reads the envelope', async () => {
    const conn = aConn()
    const spy = respond({
      connections: [conn],
      registryState: 'recovered',
      limits: {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: true,
    })
    const {api} = await load()

    await expect(api.listAzureConnections(USER)).resolves.toEqual({
      connections: [conn],
      registryState: 'recovered',
      limits: {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: true,
    })
    await api.listAzureConnections(USER)

    expect(call(spy)).toEqual({path: '/api/providers/azure-foundry/connections', method: 'GET', body: undefined})
    expect(spy).toHaveBeenCalledTimes(2)
  })

  it('reads a null connections slice (an empty Go list) as empty', async () => {
    respond({
      connections: null,
      registryState: 'ok',
      limits: {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: false,
    })
    const {api} = await load()
    await expect(api.listAzureConnections(ORG)).resolves.toEqual({
      connections: [],
      registryState: 'ok',
      limits: {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: false,
    })
  })

  it("keeps a recovered list file's backup path, and an absent one stays absent", async () => {
    respond({
      connections: [],
      registryState: 'recovered',
      limits: {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: false,
      recoveredCopyPath: '/cfg/azure-models.json.bad',
    })
    const {api} = await load()
    await expect(api.listAzureConnections(USER)).resolves.toMatchObject({
      registryState: 'recovered',
      recoveredCopyPath: '/cfg/azure-models.json.bad',
    })
    vi.restoreAllMocks()
    respond({
      connections: [],
      registryState: 'recovered',
      limits: {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: false,
    })
    await expect(api.listAzureConnections(USER)).resolves.not.toHaveProperty('recoveredCopyPath')
  })

  it('creates with a POST and unwraps {connection}', async () => {
    const spy = respond({connection: {id: 'abc0123456', name: 'contoso-prod'}}, 201)
    const {api} = await load()
    const input = {
      name: 'contoso-prod',
      config: {endpoint: 'https://contoso.openai.azure.com', auth: {mode: 'api_key' as const, key: 'k'}},
    }

    await expect(api.createAzureConnection(ORG, input)).resolves.toEqual({id: 'abc0123456', name: 'contoso-prod'})
    expect(call(spy)).toEqual({
      path: '/api/orgs/org%201/providers/azure-foundry/connections',
      method: 'POST',
      body: input,
    })
  })

  it('updates with a PUT (also the way to dismiss discovery)', async () => {
    const spy = respond({connection: {id: 'abc0123456', suggestDiscovery: false}})
    const {api} = await load()

    const conn = await api.updateAzureConnection(USER, 'abc0123456', {dismissDiscovery: true, updatedAt: 't1'})
    expect(conn).toEqual({id: 'abc0123456', suggestDiscovery: false})
    expect(call(spy)).toEqual({
      path: '/api/providers/azure-foundry/connections/abc0123456',
      method: 'PUT',
      body: {dismissDiscovery: true, updatedAt: 't1'},
    })
  })

  it('reads one connection in the enveloped and the bare form', async () => {
    respond({connection: aConn({name: 'a'})})
    let {api} = await load()
    await expect(api.getAzureConnection(USER, 'default')).resolves.toEqual(aConn({name: 'a'}))

    vi.restoreAllMocks()
    vi.resetModules()
    const spy = respond(aConn({name: 'b'}))
    ;({api} = await load())
    await expect(api.getAzureConnection(USER, 'default')).resolves.toEqual(aConn({name: 'b'}))
    expect(call(spy).method).toBe('GET')
  })

  it('deletes with cascade and force only when asked', async () => {
    const spy = respond({status: 'ok', deletedModels: 3, clearedKnowledgeEmbedding: true})
    const {api} = await load()

    await expect(api.deleteAzureConnection(USER, 'abc0123456', {cascade: true, force: true})).resolves.toEqual({
      status: 'ok',
      deletedModels: 3,
      clearedKnowledgeEmbedding: true,
    })
    await api.deleteAzureConnection(USER, 'abc0123456', {cascade: true})
    await api.deleteAzureConnection(USER, 'abc0123456')

    expect(call(spy, 0)).toMatchObject({
      path: '/api/providers/azure-foundry/connections/abc0123456?cascade=true&force=true',
      method: 'DELETE',
      body: undefined,
    })
    expect(call(spy, 1).path).toBe('/api/providers/azure-foundry/connections/abc0123456?cascade=true')
    expect(call(spy, 2).path).toBe('/api/providers/azure-foundry/connections/abc0123456')
  })

  it('discovers with a POST, reading null lists as empty and keeping an Azure refusal', async () => {
    const spy = respond({
      deployments: [
        {name: 'gpt-4.1', kind: 'chat', model: 'gpt-4.1', alreadyAdded: true, modelId: 'm/aaaaaaaaaa'},
        {name: 'embed', kind: 'embedding', model: 'text-embedding-3-large', alreadyAdded: false},
      ],
      agents: null,
      warnings: null,
      error: 'The caller lacks the Azure AI User role',
    })
    const {api} = await load()

    const d = await api.discoverAzureConnection(ORG, 'default')
    expect(call(spy)).toEqual({
      path: '/api/orgs/org%201/providers/azure-foundry/connections/default/discover',
      method: 'POST',
      body: undefined,
    })
    expect(d).toEqual({
      deployments: [
        {name: 'gpt-4.1', kind: 'chat', model: 'gpt-4.1', alreadyAdded: true, modelId: 'm/aaaaaaaaaa'},
        {name: 'embed', kind: 'embedding', model: 'text-embedding-3-large', alreadyAdded: false},
      ],
      agents: [],
      warnings: [],
      error: 'The caller lacks the Azure AI User role',
    })
  })

  it('keeps an unknown model as empty and leaves out an absent error', async () => {
    respond({
      deployments: [],
      agents: [
        {name: 'helper', kind: 'agent', model: 'gpt-4o', alreadyAdded: false},
        {
          name: 'flow',
          kind: 'agent',
          model: '',
          deployment: 'router',
          workflow: true,
          alreadyAdded: true,
          modelId: 'm/flow000000',
        },
      ],
      warnings: ['w'],
    })
    const {api} = await load()
    await expect(api.discoverAzureConnection(USER, 'abc0123456')).resolves.toEqual({
      deployments: [],
      agents: [
        {name: 'helper', kind: 'agent', model: 'gpt-4o', alreadyAdded: false},
        {
          name: 'flow',
          kind: 'agent',
          model: '',
          deployment: 'router',
          workflow: true,
          alreadyAdded: true,
          modelId: 'm/flow000000',
        },
      ],
      warnings: ['w'],
    })
  })
})

describe('providersApi Azure registry: models and test', () => {
  it('lists models, reading {models: null} as none', async () => {
    const spy = respond({models: null})
    const {api} = await load()
    await expect(api.listAzureModels(USER)).resolves.toEqual({models: []})
    expect(call(spy)).toEqual({path: '/api/providers/azure-foundry/models', method: 'GET', body: undefined})
  })

  it("passes the user scope's server default through, and leaves out an absent one", async () => {
    const model = aModel()
    respond({models: [model], serverDefault: 'm/bbbbbbbbbb'})
    const {api} = await load()
    await expect(api.listAzureModels(USER)).resolves.toEqual({models: [model], serverDefault: 'm/bbbbbbbbbb'})
    vi.restoreAllMocks()
    respond({models: [model]})
    await expect(api.listAzureModels(ORG)).resolves.toEqual({models: [model]})
  })

  it('creates, updates and deletes models on their routes', async () => {
    const model = {id: 'bbbbbbbbbb', modelId: 'm/bbbbbbbbbb'}
    const spy = respond({model})
    const {api} = await load()
    const input = {connectionId: 'default', kind: 'chat' as const, deployment: 'gpt-4.1', purpose: 'general' as const}

    await expect(api.createAzureModel(ORG, input)).resolves.toEqual(model)
    await expect(api.updateAzureModel(ORG, 'bbbbbbbbbb', {isDefault: true})).resolves.toEqual(model)
    await api.deleteAzureModel(ORG, 'bbbbbbbbbb', {force: true})
    await api.deleteAzureModel(ORG, 'bbbbbbbbbb')

    const base = '/api/orgs/org%201/providers/azure-foundry/models'
    expect(call(spy, 0)).toEqual({path: base, method: 'POST', body: input})
    expect(call(spy, 1)).toEqual({path: `${base}/bbbbbbbbbb`, method: 'PUT', body: {isDefault: true}})
    expect(call(spy, 2)).toMatchObject({path: `${base}/bbbbbbbbbb?force=true`, method: 'DELETE'})
    expect(call(spy, 3).path).toBe(`${base}/bbbbbbbbbb`)
  })

  it('posts every test shape to the one /test route', async () => {
    const spy = respond({ok: false, latencyMs: 12, error: 'bad key', code: 'PROVIDER_AUTH'})
    const {api} = await load()
    const draft = {
      connectionId: 'abc0123456',
      config: {endpoint: 'https://x.openai.azure.com', auth: {mode: 'api_key' as const}},
      model: {kind: 'chat' as const, deployment: 'gpt-4.1'},
    }

    await expect(api.testAzure(USER, draft)).resolves.toEqual({
      ok: false,
      latencyMs: 12,
      error: 'bad key',
      code: 'PROVIDER_AUTH',
    })
    await api.testAzure(USER, {model: {id: 'bbbbbbbbbb'}})

    expect(call(spy, 0)).toEqual({path: '/api/providers/azure-foundry/test', method: 'POST', body: draft})
    expect(call(spy, 1).body).toEqual({model: {id: 'bbbbbbbbbb'}})
  })
})

describe('providersApi Azure registry: error codes', () => {
  it('surfaces a 409 as VersionConflictError with its code and details', async () => {
    respond({code: 'DUPLICATE_MODEL', message: 'Already added', details: {existingModelId: 'm/cccccccccc'}}, 409)
    const {api, client} = await load()

    const err = await api
      .createAzureModel(USER, {connectionId: 'default', kind: 'chat', deployment: 'gpt-4.1'})
      .catch(e => e)
    expect(err).toBeInstanceOf(client.VersionConflictError)
    expect(err).toMatchObject({status: 409, code: 'DUPLICATE_MODEL', details: {existingModelId: 'm/cccccccccc'}})
  })

  it('surfaces a 400 validation code and a 403 as PermissionDeniedError', async () => {
    respond({code: 'INVALID_KIND', message: 'unknown kind'}, 400)
    let {api, client} = await load()
    const bad = await api.updateAzureModel(USER, 'bbbbbbbbbb', {kind: 'chat'}).catch(e => e)
    expect(bad).toBeInstanceOf(client.ApiError)
    expect(bad).toMatchObject({status: 400, code: 'INVALID_KIND', message: 'unknown kind'})

    vi.restoreAllMocks()
    vi.resetModules()
    respond({code: 'FORBIDDEN', message: 'admins only'}, 403)
    ;({api, client} = await load())
    const denied = await api.deleteAzureConnection(ORG, 'default').catch(e => e)
    expect(denied).toBeInstanceOf(client.PermissionDeniedError)
    expect(denied.code).toBe('FORBIDDEN')
  })
})

describe('providersApi.hasApiKey', () => {
  it('reads the {has} envelope, and a bare boolean', async () => {
    const spy = respond({has: false})
    const {api} = await load()
    await expect(api.hasApiKey('openai')).resolves.toBe(false)
    expect(call(spy)).toEqual({path: '/api/keys/has', method: 'POST', body: {provider: 'openai'}})
    spy.mockImplementation(async () => Response.json({has: true}))
    await expect(api.hasApiKey('openai')).resolves.toBe(true)
    spy.mockImplementation(async () => Response.json(true))
    await expect(api.hasApiKey('openai')).resolves.toBe(true)
    spy.mockImplementation(async () => Response.json({}))
    await expect(api.hasApiKey('openai')).resolves.toBe(false)
  })
})

describe('apiPath', () => {
  it('accepts a path of plain segments under /api/ and nothing else', async () => {
    const {apiPath} = await import('./providers')
    expect(apiPath('/api/providers/acme/start')).toBe('/api/providers/acme/start')
    expect(apiPath('/api/keys/v1.2_x-y')).toBe('/api/keys/v1.2_x-y')
    for (const bad of [
      undefined,
      '',
      '/api',
      '/api/',
      '/api//start',
      '/api/x/',
      '/apix/start',
      '/v1/providers/acme/start',
      'api/providers/acme/start',
      'https://evil.example/api/providers/acme/start',
      '//evil.example/api/providers/acme/start',
      'javascript:alert(1)',
      '/api/providers/../keys/delete',
      '/api/providers/%2e%2e/keys/delete',
      '/api/providers/acme/start?x=1',
      '/api/providers/acme/start#x',
      '/api/providers/acme start',
      '/api/providers/acme/start\n',
    ]) {
      expect(apiPath(bad), String(bad)).toBeUndefined()
    }
  })
})

describe('providersApi.getDeviceAuthUser', () => {
  it('reads the GitHub shape, derives a login from another, and keeps null as "not connected"', async () => {
    const spy = respond({login: 'octocat', name: 'Octo Cat'})
    const {api} = await load()
    await expect(api.getDeviceAuthUser('/api/providers/acme/user')).resolves.toEqual({
      login: 'octocat',
      name: 'Octo Cat',
    })
    expect(call(spy)).toMatchObject({path: '/api/providers/acme/user', method: 'GET'})
    spy.mockImplementation(async () => Response.json({id: 7, email: 'ann@example.com'}))
    await expect(api.getDeviceAuthUser('/api/providers/bcme/user')).resolves.toEqual({
      login: 'ann@example.com',
      name: 'ann@example.com',
    })
    spy.mockImplementation(async () => Response.json({name: 'Ann'}))
    await expect(api.getDeviceAuthUser('/api/providers/ccme/user')).resolves.toEqual({login: 'Ann', name: 'Ann'})
    spy.mockImplementation(async () => Response.json({id: 7}))
    await expect(api.getDeviceAuthUser('/api/providers/dcme/user')).resolves.toEqual({login: '7', name: '7'})
    spy.mockImplementation(async () => Response.json(null))
    await expect(api.getDeviceAuthUser('/api/providers/ecme/user')).resolves.toBeNull()
  })
})
