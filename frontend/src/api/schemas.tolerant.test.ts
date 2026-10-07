import {describe, it, expect, expectTypeOf, vi, afterEach} from 'vitest'
import type {z} from 'zod'
import {mockFetch} from '@/testing/testHelpers'
import {
  AZURE_STATUSES,
  isKnown,
  type AzureRegistryState,
  type AzureStatus,
  type ProviderID,
  type Tolerant,
} from '@/types'
import {buildSchemas} from './schemas.lazy'
import {ResponseValidationError, clearRequestCache} from './client'
import {governanceApi, type GovernanceAlertType} from './governance'
import {providersApi} from './providers'

// The wire enums a newer server may extend (types/tolerant.ts): a value this
// bundle does not know parses as itself instead of failing the whole response,
// and the known ones keep their literal types.
vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 'mock-token', port: 9999}),
  }),
}))

const s = buildSchemas()

const model = (over: Record<string, unknown> = {}) => ({
  id: 'm/abc',
  displayName: 'Model',
  contextLimit: 128000,
  inputCostPerM: 1,
  outputCostPerM: 2,
  supportsTools: true,
  ...over,
})

const provider = (over: Record<string, unknown> = {}) => ({
  id: 'claude',
  name: 'Claude',
  authType: 'api_key',
  contextLimit: 200000,
  models: [model()],
  defaultModel: 'm/abc',
  configured: true,
  ...over,
})

const conn = (over: Record<string, unknown> = {}) => ({
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

const entry = (over: Record<string, unknown> = {}) => ({
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
  updatedAt: '2026-01-01T00:00:00Z',
  ...over,
})

const alert = (over: Record<string, unknown> = {}) => ({
  id: 'a1',
  flowId: 'f1',
  type: 'drift',
  title: 'Drift detected',
  severity: 'warning',
  createdAt: '2026-01-01T00:00:00Z',
  ...over,
})

afterEach(() => {
  vi.restoreAllMocks()
  clearRequestCache()
})

describe('tolerant wire enums', () => {
  it('parses a provider listing that names a provider, sign-in type, model kind and status it does not know', () => {
    const listing = [
      provider(),
      provider({
        id: 'mistral',
        name: 'Mistral',
        authType: 'workload_identity',
        models: [model({kind: 'realtime', status: 'quota_exhausted', purpose: 'translation'})],
      }),
    ]
    const out = s.ProviderListSchema.parse(listing)
    expect(out).toEqual(listing)
    expect(out.map(p => p.id)).toEqual(['claude', 'mistral'])
    expect(out[1]!.models[0]).toMatchObject({kind: 'realtime', status: 'quota_exhausted', purpose: 'translation'})
    expectTypeOf(out[0]!.id).toEqualTypeOf<Tolerant<ProviderID>>()
  })

  it('still rejects a value that is not a string', () => {
    expect(s.ProviderListSchema.safeParse([provider({authType: 42})]).success).toBe(false)
    expect(s.AzureModelsListingSchema.safeParse({models: [entry({kind: null})]}).success).toBe(false)
  })

  it('parses the Azure registry with an unknown status, sign-in method, registry state, kind and purpose', () => {
    const connections = s.AzureConnectionsListingSchema.parse({
      connections: [
        conn({status: 'suspended', config: {endpoint: 'https://x', authMode: 'managed_identity', hasSecret: false}}),
      ],
      registryState: 'migrating',
      limits: {connections: 10, models: 50},
      knowledgeEmbeddingEnabled: false,
    })
    expect(connections.registryState).toBe('migrating')
    expect(connections.connections[0]).toMatchObject({status: 'suspended', config: {authMode: 'managed_identity'}})
    // Known values keep their literal types: the guard narrows to the closed union.
    expectTypeOf(connections.registryState).toEqualTypeOf<Tolerant<AzureRegistryState>>()
    const status = connections.connections[0]!.status
    expect(isKnown(AZURE_STATUSES, status)).toBe(false)
    if (isKnown(AZURE_STATUSES, status)) expectTypeOf(status).toEqualTypeOf<AzureStatus>()

    const models = s.AzureModelsListingSchema.parse({
      models: [entry(), entry({id: 'cccccccccc', kind: 'realtime', purpose: 'translation', status: 'suspended'})],
    })
    expect(models.models.map(m => [m.kind, m.purpose, m.status])).toEqual([
      ['chat', 'general', 'ready'],
      ['realtime', 'translation', 'suspended'],
    ])
  })

  it('leaves a discovered item of an unknown kind out of the suggestions: it could not be added', () => {
    const out = s.AzureDiscoverySchema.parse({
      deployments: [
        {name: 'prod-chat', kind: 'chat', model: 'gpt-4.1', alreadyAdded: false},
        {name: 'live-voice', kind: 'realtime', model: 'gpt-rt', alreadyAdded: false},
      ],
      agents: null,
      warnings: [],
    })
    expect(out.deployments.map(d => d.name)).toEqual(['prod-chat'])
    expect(out.agents).toEqual([])
  })

  it('keeps an alert of an unknown type and severity, typed tolerant', () => {
    const out = s.GovernanceAlertsSchema.parse([
      alert(),
      alert({id: 'a2', type: 'policy_failed', severity: 'critical'}),
    ])
    expect(out.map(a => [a.type, a.severity])).toEqual([
      ['drift', 'warning'],
      ['policy_failed', 'critical'],
    ])
    type Alert = z.output<typeof s.GovernanceAlertsSchema>[number]
    expectTypeOf<Alert['type']>().toEqualTypeOf<Tolerant<GovernanceAlertType>>()
  })

  it('keeps the role closed: an unknown role is refused, never guessed into a privilege', () => {
    const user = {id: 'u1', email: 'a@b.c', role: 'owner'}
    expect(s.AuthUserSchema.safeParse(user).success).toBe(false)
  })

  describe('through the client', () => {
    it('one alert of a newer type no longer empties the inbox', async () => {
      mockFetch([alert(), alert({id: 'a2', type: 'policy_failed'})])
      await expect(governanceApi.list()).resolves.toHaveLength(2)
    })

    it('one provider a newer server added no longer blanks the listing', async () => {
      mockFetch([provider(), provider({id: 'mistral', name: 'Mistral', authType: 'workload_identity'})])
      const out = await providersApi.listProviders()
      expect(out.map(p => p.id)).toEqual(['claude', 'mistral'])
    })

    it('a value of the wrong type is still drift', async () => {
      mockFetch([provider({id: 7})])
      await expect(providersApi.listProviders()).rejects.toBeInstanceOf(ResponseValidationError)
    })
  })
})
