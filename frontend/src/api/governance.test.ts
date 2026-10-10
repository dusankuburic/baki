import {describe, it, expect, vi, afterEach} from 'vitest'
import type {OrgCustomRuleConfig} from './governance'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({
      apiUrl: 'http://localhost:9999',
      token: 'mock-token',
      port: 9999,
    }),
  }),
}))

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {status, headers: {'Content-Type': 'application/json'}})

const config: OrgCustomRuleConfig = {
  kind: 'block',
  schemaVersion: 1,
  id: 'house',
  name: 'House',
  severity: 'warning',
  rawTypeMatch: '^SET$',
}

async function getOrgRulesApi() {
  return (await import('./governance')).orgRulesApi
}

describe('orgRulesApi.setEnabled', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  it('pauses with the row id only', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(json({id: 'row-1'}))
    const api = await getOrgRulesApi()

    await api.setEnabled('org-1', 'row-1', false, config)

    expect(fetchSpy).toHaveBeenCalledTimes(1)
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toContain('/api/orgs/org-1/rules')
    expect(JSON.parse(init.body as string)).toEqual({id: 'row-1', enabled: false})
  })

  // A server rolled back to before rule schemas toggles only with the config.
  it('resends the config when an older server requires it', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(
        json({code: 'BAD_REQUEST', message: 'config.id is required — it is the rule id that appears on findings'}, 400),
      )
      .mockResolvedValueOnce(json({id: 'row-1'}))
    const api = await getOrgRulesApi()

    await api.setEnabled('org-1', 'row-1', true, config)

    expect(fetchSpy).toHaveBeenCalledTimes(2)
    const [, init] = fetchSpy.mock.calls[1] as [string, RequestInit]
    expect(JSON.parse(init.body as string)).toEqual({id: 'row-1', config, enabled: true})
  })

  it('does not retry any other refusal', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(json({code: 'BAD_REQUEST', message: 'invalid rule'}, 400))
    const api = await getOrgRulesApi()

    await expect(api.setEnabled('org-1', 'row-1', true, config)).rejects.toThrow('invalid rule')
    expect(fetchSpy).toHaveBeenCalledTimes(1)
  })
})
