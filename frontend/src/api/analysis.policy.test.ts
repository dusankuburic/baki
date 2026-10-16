import {describe, it, expect, expectTypeOf, vi, afterEach} from 'vitest'
import {mockFetch} from '@/testing/testHelpers'
import type {Policy, PolicyRule, PolicyWaiver, Severity, Tolerant} from '@/types'
import {analysisApi} from './analysis'
import {clearRequestCache} from './client'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 'mock-token', port: 9999}),
  }),
}))

afterEach(() => {
  vi.restoreAllMocks()
  clearRequestCache()
})

// A policy as the server stores it since v36 (models.Policy): waivers, per-severity budgets and a per-rule cap.
const stored = {
  id: 'p1',
  orgId: 'org-1',
  name: 'Release gate',
  rules: [{ruleId: 'todo-comment', enabled: true, severity: 'warning', maxCount: 3}],
  gateSeverity: 'error',
  waivers: [{findingKey: 'fp-1', reason: 'accepted risk', expiresAt: '2027-01-01T00:00:00Z'}, {findingKey: 'fp-2'}],
  maxErrors: 0,
  maxWarnings: 5,
  maxInfo: 10,
  createdAt: '2026-10-01T10:00:00Z',
  updatedAt: '2026-10-02T10:00:00Z',
}

describe('policies through the client', () => {
  it('keeps the waivers and budgets a policy is stored with', async () => {
    mockFetch([stored])
    await expect(analysisApi.listPolicies('org-1')).resolves.toEqual([stored])
    mockFetch(stored)
    await expect(analysisApi.savePolicy(stored as Policy)).resolves.toEqual(stored)
  })

  it('tolerates what another server release may send, and keeps it for the next save', async () => {
    mockFetch([
      {
        ...stored,
        gateSeverity: 'critical',
        rules: [{ruleId: 'todo-comment', enabled: true, severity: 'fatal'}],
        maxWarnings: null,
        futureField: {x: 1},
      },
      // A policy saved through the API without rules: the server's nil slice is a JSON null.
      {id: 'p2', orgId: 'org-1', name: 'Bare', rules: null},
    ])
    const [first, bare] = await analysisApi.listPolicies('org-1')
    expect(first!.gateSeverity).toBe('critical')
    expect(first!.rules[0]!.severity).toBe('fatal')
    expect(first!.maxWarnings).toBeUndefined()
    expect(first).toHaveProperty('futureField', {x: 1})
    expect(first!.waivers).toEqual(stored.waivers)
    expect(bare!.rules).toEqual([])
  })

  it('types the waivers, the budgets and the tolerant severities', () => {
    expectTypeOf<Policy['waivers']>().toEqualTypeOf<PolicyWaiver[] | undefined>()
    expectTypeOf<Policy['maxErrors']>().toEqualTypeOf<number | undefined>()
    expectTypeOf<Policy['maxWarnings']>().toEqualTypeOf<number | undefined>()
    expectTypeOf<Policy['maxInfo']>().toEqualTypeOf<number | undefined>()
    expectTypeOf<PolicyRule['maxCount']>().toEqualTypeOf<number | undefined>()
    expectTypeOf<Policy['gateSeverity']>().toEqualTypeOf<Tolerant<Severity> | '' | undefined>()
    expectTypeOf<PolicyRule['severity']>().toEqualTypeOf<Tolerant<Severity> | '' | undefined>()
  })
})
