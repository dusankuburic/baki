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

describe('orgRulesApi.list', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  it('is never answered from the GET micro-cache', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(json([]))
      .mockResolvedValueOnce(json([{id: 'row-1'}]))
    const api = await getOrgRulesApi()

    expect(await api.list('org-1')).toEqual([])
    expect(await api.list('org-1')).toEqual([{id: 'row-1'}])
    expect(fetchSpy).toHaveBeenCalledTimes(2)
  })
})

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

describe('ciCredentialsApi', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  const getApi = async () => (await import('./governance')).ciCredentialsApi

  const row = {
    id: 'KEYID0000000000A',
    name: 'azure-main',
    secretHint: 'abcd',
    createdBy: 'u-1',
    createdByName: 'Ada Admin',
    createdAt: '2026-01-01T00:00:00Z',
  }

  it('reads a null (Go nil slice) list as empty', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json(null))
    const api = await getApi()

    await expect(api.list('org-1')).resolves.toEqual([])
  })

  it('reads null or absent timestamps, and keeps fields a newer server adds', async () => {
    const rows = [
      {...row, expiresAt: null, lastUsedAt: null},
      {...row, id: 'KEYID0000000000B', expiresAt: '2026-02-01T00:00:00Z', lastUsedAt: '2026-01-05T00:00:00Z', x: 1},
      row,
    ]
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(json(rows))
    const api = await getApi()

    await expect(api.list('org-1')).resolves.toEqual(rows)
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toContain('/api/orgs/org-1/ci-credentials')
    expect(init.method).toBe('GET')
  })

  it('reads absent display strings (hint, creator) as empty instead of failing the list', async () => {
    const {secretHint: _h, createdBy: _c, createdByName: _n, ...bare} = row
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json([bare]))
    const api = await getApi()

    await expect(api.list('org-1')).resolves.toEqual([{...bare, secretHint: '', createdBy: '', createdByName: ''}])
  })

  it('refuses a row without an id: it could not be revoked', async () => {
    const {id: _, ...noId} = row
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json([noId]))
    const api = await getApi()

    await expect(api.list('org-1')).rejects.toMatchObject({name: 'ResponseValidationError'})
  })

  it('creates with the name and expiry, and returns the credential', async () => {
    const created = {...row, expiresAt: '2026-01-31T00:00:00Z', lastUsedAt: null, credential: 'baki_ci_KEYID_secret'}
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(json(created, 201))
    const api = await getApi()

    await expect(api.create('org-1', {name: 'azure-main', expiresInDays: 30})).resolves.toEqual(created)
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toContain('/api/orgs/org-1/ci-credentials')
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body as string)).toEqual({name: 'azure-main', expiresInDays: 30})
  })

  it('refuses a create answer without the credential: there would be nothing to copy', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json(row, 201))
    const api = await getApi()

    await expect(api.create('org-1', {name: 'azure-main'})).rejects.toMatchObject({name: 'ResponseValidationError'})
  })

  it('revokes with a DELETE answered 204', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(null, {status: 204}))
    const api = await getApi()

    await expect(api.revoke('org-1', 'KEYID0000000000A')).resolves.toBeUndefined()
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toContain('/api/orgs/org-1/ci-credentials/KEYID0000000000A')
    expect(init.method).toBe('DELETE')
  })
})

describe('channelsApi', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  const getApi = async () => (await import('./governance')).channelsApi

  const row = {
    id: 'c1',
    name: 'Ops',
    kind: 'slack',
    url: 'hooks.slack.com/…',
    enabled: true,
    createdAt: '2026-01-01T00:00:00Z',
    hasSecret: true,
  }

  it('reads a null (Go nil slice) list as empty', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json(null))
    const api = await getApi()

    await expect(api.list('org-1')).resolves.toEqual([])
  })

  it('reads a Teams Workflows channel as a kind it knows', async () => {
    const rows = [{...row, kind: 'teams-workflow', hasSecret: false}]
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json(rows))
    const api = await getApi()
    const {CHANNEL_KINDS} = await import('./governance')

    const out = await api.list('org-1')
    expect(out[0]?.kind).toBe('teams-workflow')
    expect(CHANNEL_KINDS).toContain(out[0]?.kind)
  })

  it('keeps a channel of a kind a newer server added, and one from a server that sends no hasSecret', async () => {
    const {hasSecret: _, ...older} = row
    const rows = [row, {...row, id: 'c2', kind: 'pagerduty', x: 1}, {...older, id: 'c3'}]
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json(rows))
    const api = await getApi()

    const out = await api.list('org-1')
    expect(out).toEqual(rows)
    expect(out[2]).not.toHaveProperty('hasSecret')
  })

  it('refuses a row without an id: it could not be edited or removed', async () => {
    const {id: _, ...noId} = row
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json([noId]))
    const api = await getApi()

    await expect(api.list('org-1')).rejects.toMatchObject({name: 'ResponseValidationError'})
  })

  it('reads the list from the server every time', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(json([row]))
      .mockResolvedValueOnce(json([{...row, enabled: false}]))
    const api = await getApi()

    await api.list('org-1')
    await expect(api.list('org-1')).resolves.toMatchObject([{enabled: false}])
    expect(fetchSpy).toHaveBeenCalledTimes(2)
  })

  it('posts the save body as built', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(json(row))
    const api = await getApi()
    const body = {
      id: 'c1',
      name: 'Ops',
      kind: 'slack' as const,
      url: row.url,
      urlUnchanged: true,
      clear: ['secret' as const],
    }

    await api.save('org-1', body)
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toContain('/api/orgs/org-1/channels')
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body as string)).toEqual(body)
  })
})

describe('channel save bodies', () => {
  const ch = {
    id: 'c1',
    name: 'Ops',
    kind: 'webhook' as const,
    url: 'hooks.example.com/…',
    enabled: false,
    createdAt: '',
    hasSecret: true,
  }
  const edit = {name: 'Ops', url: '', secret: '', clearSecret: false}

  it('toggles with the listed URL as unchanged and no secret', async () => {
    const {channelToggleBody} = await import('./governance')
    expect(channelToggleBody(ch, true)).toEqual({
      id: 'c1',
      name: 'Ops',
      kind: 'webhook',
      url: 'hooks.example.com/…',
      urlUnchanged: true,
      enabled: true,
    })
  })

  it('edits keeping the URL, the secret and the enabled state unless changed', async () => {
    const {channelEditBody} = await import('./governance')
    expect(channelEditBody(ch, edit)).toEqual({
      id: 'c1',
      name: 'Ops',
      kind: 'webhook',
      url: 'hooks.example.com/…',
      urlUnchanged: true,
      enabled: false,
    })
    expect(channelEditBody(ch, {...edit, url: ' https://new.example.com/x ', secret: ' k '})).toEqual({
      id: 'c1',
      name: 'Ops',
      kind: 'webhook',
      url: 'https://new.example.com/x',
      secret: 'k',
      enabled: false,
    })
    expect(channelEditBody(ch, {...edit, secret: '   '})).not.toHaveProperty('secret')
  })

  it('clears the secret instead of sending a typed one: setting and clearing at once is refused', async () => {
    const {channelEditBody} = await import('./governance')
    const body = channelEditBody(ch, {...edit, secret: 'typed', clearSecret: true})
    expect(body.clear).toEqual(['secret'])
    expect(body).not.toHaveProperty('secret')
  })

  it('never sends a secret for Teams, which has none', async () => {
    const {channelCreateBody, channelEditBody} = await import('./governance')
    const teams = {...ch, kind: 'teams' as const}
    const body = channelEditBody(teams, {...edit, secret: 'typed', clearSecret: true})
    expect(body).not.toHaveProperty('secret')
    expect(body).not.toHaveProperty('clear')
    expect(
      channelCreateBody({name: ' Room ', kind: 'teams', url: ' https://x.example.com/y ', secret: 'typed'}),
    ).toEqual({name: 'Room', kind: 'teams', url: 'https://x.example.com/y'})
  })

  it('never sends a secret for Teams Workflows either', async () => {
    const {channelCreateBody, channelEditBody, channelSigns} = await import('./governance')
    expect(channelSigns('teams-workflow')).toBe(false)
    const body = channelEditBody(
      {...ch, kind: 'teams-workflow' as const},
      {...edit, secret: 'typed', clearSecret: true},
    )
    expect(body).not.toHaveProperty('secret')
    expect(body).not.toHaveProperty('clear')
    expect(
      channelCreateBody({name: 'Room', kind: 'teams-workflow', url: 'https://x.example.com/y', secret: 'typed'}),
    ).toEqual({name: 'Room', kind: 'teams-workflow', url: 'https://x.example.com/y'})
  })
})

describe('Jira and email channels, events, options', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.resetModules()
  })

  const base = {id: 'c1', name: 'Ops', url: '', enabled: true, createdAt: '2026-01-01T00:00:00Z', hasSecret: false}
  const edit = {name: 'Ops', url: '', secret: '', clearSecret: false}

  it('reads Jira and email rows with their fields, events and the unreadable flag', async () => {
    const rows = [
      {
        ...base,
        kind: 'jira',
        url: 'acme.atlassian.net/…',
        email: 'bot@acme.io',
        project: 'PAD',
        hasToken: true,
        events: ['drift'],
      },
      {...base, id: 'c2', kind: 'email', recipients: ['u1', 'u2'], events: ['new_rules', 'paged']},
      {...base, id: 'c3', kind: 'webhook', url: '', unreadable: true},
    ]
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json(rows))
    const out = await (await import('./governance')).channelsApi.list('org-1')
    expect(out).toEqual(rows)
  })

  it('reads the options', async () => {
    const options = {kinds: ['webhook', 'jira'], events: ['drift'], defaultEvents: ['drift'], hostsRestricted: true}
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json(options))
    const {channelsApi} = await import('./governance')
    await expect(channelsApi.options('org-1')).resolves.toEqual(options)
  })

  it('reads null from a server without the options route', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(json({error: 'Method Not Allowed'}, 405))
    const {channelsApi} = await import('./governance')
    await expect(channelsApi.options('org-1')).resolves.toBeNull()
  })

  it('creates a Jira channel with its fields and an email channel without a URL', async () => {
    const {channelCreateBody} = await import('./governance')
    expect(
      channelCreateBody({
        name: ' Tickets ',
        kind: 'jira',
        url: ' https://acme.atlassian.net ',
        secret: 'ignored',
        email: ' bot@acme.io ',
        token: ' tok ',
        project: ' PAD ',
        recipients: ['ignored'],
      }),
    ).toEqual({
      name: 'Tickets',
      kind: 'jira',
      url: 'https://acme.atlassian.net',
      email: 'bot@acme.io',
      token: 'tok',
      project: 'PAD',
    })
    expect(
      channelCreateBody({
        name: 'Leads',
        kind: 'email',
        url: 'typed',
        secret: '',
        recipients: ['u1'],
        events: ['drift'],
      }),
    ).toEqual({name: 'Leads', kind: 'email', recipients: ['u1'], events: ['drift']})
  })

  it('edits a Jira channel keeping the token unless typed, and toggles an email channel without a URL', async () => {
    const {channelEditBody, channelToggleBody} = await import('./governance')
    const jira = {
      ...base,
      kind: 'jira' as const,
      url: 'acme.atlassian.net/…',
      email: 'bot@acme.io',
      project: 'PAD',
      hasToken: true,
    }
    expect(channelEditBody(jira, {...edit, email: 'bot@acme.io', project: 'OPS', token: '  '})).toEqual({
      id: 'c1',
      name: 'Ops',
      kind: 'jira',
      url: 'acme.atlassian.net/…',
      urlUnchanged: true,
      enabled: true,
      email: 'bot@acme.io',
      project: 'OPS',
    })
    const mail = {...base, kind: 'email' as const, recipients: ['u1']}
    expect(channelToggleBody(mail, false)).toEqual({id: 'c1', name: 'Ops', kind: 'email', enabled: false})
    expect(channelEditBody(mail, {...edit, recipients: ['u1', 'u2'], events: ['new_rules']})).toEqual({
      id: 'c1',
      name: 'Ops',
      kind: 'email',
      enabled: true,
      recipients: ['u1', 'u2'],
      events: ['new_rules'],
    })
  })
})
