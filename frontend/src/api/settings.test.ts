import {describe, it, expect, vi, afterEach} from 'vitest'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 'mock-token', port: 9999}),
  }),
}))

function respond(payload: unknown, status = 200) {
  return vi
    .spyOn(globalThis, 'fetch')
    .mockImplementation(
      async () => new Response(JSON.stringify(payload), {status, headers: {'Content-Type': 'application/json'}}),
    )
}

function call(spy: ReturnType<typeof respond>, i = 0) {
  const [url, init] = spy.mock.calls[i] as [string, RequestInit]
  return {
    path: url.replace('http://localhost:9999', ''),
    method: init.method,
    body: init.body === undefined ? undefined : JSON.parse(init.body as string),
  }
}

afterEach(async () => {
  vi.restoreAllMocks()
  ;(await import('./client')).clearRequestCache()
})

// R-config-4: the section route's wire shape.
describe('settingsApi org sections', () => {
  const section = {section: 'ai', values: {allowedProviders: ['openai']}, revision: 'r1'}

  it('reads a section past the GET cache, so its revision is current', async () => {
    const spy = respond(section)
    const {settingsApi} = await import('./settings')
    expect(await settingsApi.getOrgSettingsSection('org-1', 'ai')).toEqual(section)
    expect(await settingsApi.getOrgSettingsSection('org-1', 'ai')).toEqual(section)
    expect(spy).toHaveBeenCalledTimes(2)
    expect(call(spy)).toEqual({path: '/api/system/settings/org/org-1/sections/ai', method: 'GET', body: undefined})
  })

  it('patches the named fields with the revision read, and without one when none is given', async () => {
    const spy = respond({...section, revision: 'r2'})
    const {settingsApi} = await import('./settings')
    const out = await settingsApi.patchOrgSettingsSection('org-1', 'ai', {allowedProviders: ['openai']}, 'r1')
    expect(out.revision).toBe('r2')
    expect(call(spy)).toEqual({
      path: '/api/system/settings/org/org-1/sections/ai',
      method: 'PATCH',
      body: {values: {allowedProviders: ['openai']}, ifRevision: 'r1'},
    })
    await settingsApi.patchOrgSettingsSection('org-1', 'ai', {toolsOffByDefault: true})
    expect(call(spy, 1).body).toEqual({values: {toolsOffByDefault: true}})
  })

  it('surfaces a stale revision as ApiError 409 SETTINGS_STALE', async () => {
    respond({error: 'the settings changed since they were read', code: 'SETTINGS_STALE'}, 409)
    const {settingsApi} = await import('./settings')
    const {ApiError} = await import('./client')
    const err = await settingsApi.patchOrgSettingsSection('org-1', 'ai', {}, 'r0').catch(e => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({status: 409, code: 'SETTINGS_STALE'})
  })
})
