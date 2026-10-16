import {describe, it, expect, vi, afterEach} from 'vitest'
import {mockFetch} from '@/testing/testHelpers'
import {ResponseValidationError, clearRequestCache} from './client'
import {flowApi} from './flow'
import {libraryApi} from './library'
import {governanceApi} from './governance'
import {providersApi} from './providers'
import {chatApi} from './chat'
import {adminApi} from './admin'
import {dashboardApi} from './dashboard'
import {systemApi} from './system'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({
      apiUrl: 'http://localhost:9999',
      token: 'mock-token',
      port: 9999,
    }),
  }),
}))

describe('api read validation (per schema namespace)', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    clearRequestCache()
  })

  describe('flow', () => {
    it('accepts a well-formed recent-files list', async () => {
      const row = {path: 'C:\\flows\\a.txt', name: 'a.txt', size: 12, lastOpen: '2026-01-01T00:00:00Z', isFolder: false}
      mockFetch([row])
      await expect(flowApi.recentFiles()).resolves.toEqual([row])
    })

    it('reads a null (Go nil slice) recent list as empty', async () => {
      mockFetch(null)
      await expect(flowApi.recentFiles()).resolves.toEqual([])
    })

    it('reads null search results as an empty array', async () => {
      mockFetch({
        query: {text: 'x', fuzzy: false, maxResults: 50},
        results: null,
        totalCount: 0,
        durationMs: 3,
      })
      const out = await flowApi.searchFlow('f1', {text: 'x', fuzzy: false, maxResults: 50})
      expect(out.results).toEqual([])
    })

    it('throws ResponseValidationError when a loaded document misses parser-emitted fields', async () => {
      mockFetch({id: 'f1', name: 'Flow', subflows: [{id: 's1', name: 'Main'}]})
      await expect(flowApi.loadFlowFromPath('C:\\flows\\a.txt')).rejects.toBeInstanceOf(ResponseValidationError)
    })
  })

  describe('library', () => {
    const row = {
      id: 'f1',
      name: 'Flow',
      ownerId: 'u1',
      isSharedWithMe: false,
      canEdit: true,
      canDelete: true,
      canShare: true,
      blockCount: 5,
      subflowCount: 1,
      updatedAt: '2026-01-01T00:00:00Z',
      version: 1,
    }

    it('accepts a well-formed paged library list', async () => {
      mockFetch({items: [row], total: 1, offset: 0, limit: 50})
      const out = await libraryApi.list()
      expect(out.items).toHaveLength(1)
      expect(out.total).toBe(1)
    })

    it('throws ResponseValidationError when a list row misses a permission flag', async () => {
      mockFetch({items: [{...row, canEdit: undefined}], total: 1, offset: 0, limit: 50})
      await expect(libraryApi.list()).rejects.toBeInstanceOf(ResponseValidationError)
    })

    it('accepts a well-formed version history', async () => {
      const version = {id: 'v1', flowId: 'f1', version: 1, comment: '', createdBy: 'u1', createdAt: '2026-01-01T00:00:00Z'}
      mockFetch([version])
      await expect(libraryApi.versions('f1')).resolves.toEqual([version])
    })
  })

  describe('governance', () => {
    const alert = {
      id: 'a1',
      flowId: 'f1',
      type: 'drift',
      title: 'Drift detected',
      severity: 'warning',
      createdAt: '2026-01-01T00:00:00Z',
    }

    it('accepts a well-formed alerts list and unread count', async () => {
      mockFetch([alert])
      await expect(governanceApi.list()).resolves.toEqual([alert])
      mockFetch({count: 3})
      await expect(governanceApi.unreadCount()).resolves.toEqual({count: 3})
    })

    it('throws ResponseValidationError on a severity that is not a string', async () => {
      mockFetch([{...alert, severity: 2}])
      await expect(governanceApi.list()).rejects.toBeInstanceOf(ResponseValidationError)
    })
  })

  describe('providers', () => {
    const provider = {
      id: 'claude',
      name: 'Claude',
      authType: 'api_key',
      contextLimit: 200000,
      models: [
        {id: 'm1', displayName: 'Claude X', contextLimit: 200000, inputCostPerM: 3, outputCostPerM: 15, supportsTools: true},
      ],
      defaultModel: 'm1',
      configured: true,
    }

    it('accepts a well-formed provider list', async () => {
      mockFetch([provider])
      const out = await providersApi.listProviders()
      expect(out).toEqual([provider])
    })

    it('throws ResponseValidationError when a model entry misses its display name', async () => {
      mockFetch([
        {...provider, models: [{id: 'm1', contextLimit: 200000, inputCostPerM: 3, outputCostPerM: 15, supportsTools: true}]},
      ])
      await expect(providersApi.listProviders()).rejects.toBeInstanceOf(ResponseValidationError)
    })

    it('accepts null as "not connected" for the GitHub/Copilot user', async () => {
      mockFetch(null)
      await expect(providersApi.getGitHubUser()).resolves.toBeNull()
      mockFetch(null)
      await expect(providersApi.getCopilotUser()).resolves.toBeNull()
    })

    it('accepts the Azure connections listing with a nil-marshalled connections slice', async () => {
      mockFetch({
        connections: null,
        registryState: 'ok',
        limits: {connections: 10, models: 50},
        knowledgeEmbeddingEnabled: false,
      })
      const out = await providersApi.listAzureConnections({kind: 'user'})
      expect(out.connections).toEqual([])
    })

    it('throws ResponseValidationError when a discovery item misses its kind', async () => {
      mockFetch({deployments: [{name: 'prod-chat', model: 'gpt-4.1', alreadyAdded: false}], agents: [], warnings: []})
      await expect(providersApi.discoverAzureConnection({kind: 'user'}, 'default')).rejects.toBeInstanceOf(
        ResponseValidationError,
      )
    })
  })

  describe('chat', () => {
    it('accepts a suggested-prompts list and reads null as empty', async () => {
      mockFetch(['Explain this flow'])
      await expect(chatApi.getSuggestedPrompts(true)).resolves.toEqual(['Explain this flow'])
      mockFetch(null)
      await expect(chatApi.getSuggestedPrompts(false)).resolves.toEqual([])
    })

    it('throws ResponseValidationError when a prompt is not a string', async () => {
      mockFetch([42])
      await expect(chatApi.getSuggestedPrompts(true)).rejects.toBeInstanceOf(ResponseValidationError)
    })
  })

  describe('admin', () => {
    const user = {
      id: 'u1',
      email: 'a@b.c',
      role: 'member',
      emailVerified: true,
      displayName: 'A',
      avatarUrl: '',
      createdAt: '2026-01-01T00:00:00Z',
      updatedAt: '2026-01-01T00:00:00Z',
    }

    it('accepts a well-formed users page and stats', async () => {
      mockFetch({items: [user], total: 1, offset: 0, limit: 50})
      const page = await adminApi.listUsers({limit: 50, offset: 0})
      expect(page.items).toEqual([user])

      mockFetch({items: [user], total: 2, offset: 0, limit: 1, hasMore: true, nextCursor: 'c1'})
      expect(await adminApi.listUsers({limit: 1, offset: 0})).toMatchObject({hasMore: true, nextCursor: 'c1'})
      mockFetch({items: [user], total: 2, offset: 0, limit: 1, hasMore: true, nextCursor: 7})
      await expect(adminApi.listUsers({limit: 1, offset: 0})).rejects.toBeInstanceOf(ResponseValidationError)

      mockFetch({total: 1, byRole: {admin: 0, member: 1, viewer: 0, guest: 0, none: 0}, unverified: 0, locked: 0})
      await expect(adminApi.userStats()).resolves.toEqual({
        total: 1,
        byRole: {admin: 0, member: 1, viewer: 0, guest: 0, none: 0},
        unverified: 0,
        locked: 0,
      })
    })

    it('throws ResponseValidationError on an unknown role value', async () => {
      mockFetch({items: [{...user, role: 'boss'}], total: 1, offset: 0, limit: 50})
      await expect(adminApi.listUsers({limit: 50, offset: 0})).rejects.toBeInstanceOf(ResponseValidationError)
    })

    it('accepts an audit page with a null meta map', async () => {
      const event = {
        id: 'e1',
        userId: 'u1',
        email: 'a@b.c',
        action: 'auth.login',
        resourceType: 'user',
        resourceId: 'u1',
        ip: '127.0.0.1',
        meta: null,
        createdAt: '2026-01-01T00:00:00Z',
      }
      mockFetch([event])
      await expect(adminApi.listAuditEvents()).resolves.toEqual({events: [event], nextCursor: undefined})
    })

    it("hands back the audit page's X-Next-Cursor, the next page's cursor", async () => {
      const headers = {'Content-Type': 'application/json', 'X-Next-Cursor': 'c-2'}
      const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response('[]', {status: 200, headers}))
      const page = await adminApi.listAuditEvents({limit: 15, cursor: 'c-1'})
      expect(page).toEqual({events: [], nextCursor: 'c-2'})
      expect(String(spy.mock.calls[0]![0])).toContain('cursor=c-1')
    })
  })

  describe('dashboard', () => {
    const home = {
      greeting: {userDisplayName: 'Dev'},
      overview: {avgHealthScore: 90, healthAvailable: true, totalFlows: 2, totalSubflows: 4},
      tokenUsage: null,
      recentFlows: [{id: 'f1', name: 'Flow', healthScore: null, updatedAt: '2026-01-01T00:00:00Z'}],
      findings: {available: false, bySeverity: {}, byCategory: []},
      isCloud: false,
      healthTrend: null,
      costByProvider: null,
      ruleFrequency: null,
      activity: null,
      complexity: null,
      security: {available: false, failedLogins24h: 0, credentialFindings: 0, admin: true},
      severityTrend: null,
      confidenceDist: null,
      healthBuckets: null,
      fixability: {available: 0, total: 0, autoFixableRules: 0, totalRules: 0},
      workflow: {available: false},
    }

    it('accepts the home payload and reads its nil collections as empty', async () => {
      mockFetch(home)
      const out = await dashboardApi.getHome()
      expect(out.tokenUsage).toEqual([])
      expect(out.confidenceDist).toEqual({})
      expect(out.recentFlows).toHaveLength(1)
    })

    it('throws ResponseValidationError when a home section is missing', async () => {
      const {overview: _overview, ...withoutOverview} = home
      mockFetch(withoutOverview)
      await expect(dashboardApi.getHome()).rejects.toBeInstanceOf(ResponseValidationError)
    })

    it('accepts a minimal analytics payload', async () => {
      mockFetch({
        applied: {scope: 'all', compare: true, derived: false},
        range: {preset: '30d', from: '2026-01-01', to: '2026-01-31'},
        summary: {asOf: '2026-01-31', flowsInScope: 2, unanalyzed: 0},
        trend: {points: [], prevPoints: []},
        findings: {},
        flows: {},
        triage: {},
        ai: {},
        activity: {},
        security: {available: false},
        facets: {flowsTruncated: false},
        partial: [],
        generatedAt: '2026-01-31T00:00:00Z',
      })
      const out = await dashboardApi.getAnalytics('preset=30d')
      expect(out.summary.flowsInScope).toBe(2)
    })

    it('throws ResponseValidationError when the analytics payload misses a section', async () => {
      mockFetch({applied: {scope: 'all'}, generatedAt: '2026-01-31T00:00:00Z'})
      await expect(dashboardApi.getAnalytics('preset=30d')).rejects.toBeInstanceOf(ResponseValidationError)
    })
  })

  describe('system', () => {
    const info = {
      version: '1.2.3',
      platform: 'windows',
      arch: 'amd64',
      buildDate: '2026-01-01',
      gitCommit: 'deadbeef',
      capabilities: {sessionAnalytics: true},
    }

    it('accepts a well-formed app info', async () => {
      mockFetch(info)
      await expect(systemApi.appInfo()).resolves.toEqual(info)
    })

    it('throws ResponseValidationError when capabilities are missing', async () => {
      const {capabilities: _capabilities, ...withoutCapabilities} = info
      mockFetch(withoutCapabilities)
      await expect(systemApi.appInfo()).rejects.toBeInstanceOf(ResponseValidationError)
    })
  })
})
