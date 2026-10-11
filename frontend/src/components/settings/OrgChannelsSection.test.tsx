import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {ApiError} from '@/api/client'
import {channelsApi, type ChannelOptions, type OrgChannel} from '@/api/governance'
import type {OrgMember} from '@/stores/orgStore'
import {unknownValue} from '@/testing/testHelpers'
import OrgChannelsSection from './OrgChannelsSection'

vi.mock('@/api/governance', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/governance')>()),
  channelsApi: {list: vi.fn(), options: vi.fn(), save: vi.fn(), remove: vi.fn(), test: vi.fn()},
}))

const api = vi.mocked(channelsApi)

const channel = (over: Partial<OrgChannel> = {}): OrgChannel => ({
  id: 'c1',
  name: 'Ops hook',
  kind: 'teams',
  url: 'hooks.example.com/…',
  enabled: true,
  createdAt: '2026-01-01T00:00:00Z',
  hasSecret: false,
  ...over,
})

const webhook = (over: Partial<OrgChannel> = {}) =>
  channel({id: 'w1', name: 'Build hook', kind: 'webhook', hasSecret: true, ...over})

const members: OrgMember[] = [
  {
    userId: 'ann',
    role: 'admin',
    joinedAt: '',
    user: {id: 'ann', email: 'ann@acme.io', displayName: 'Ann', avatarUrl: ''},
  },
  {
    userId: 'bob',
    role: 'member',
    joinedAt: '',
    user: {id: 'bob', email: 'bob@acme.io', displayName: '', avatarUrl: ''},
  },
]

const fullOptions = (over: Partial<ChannelOptions> = {}): ChannelOptions => ({
  kinds: ['email', 'jira', 'slack', 'teams', 'teams-workflow', 'webhook'],
  events: ['drift', 'health_regression', 'new_rules'],
  defaultEvents: ['drift', 'health_regression', 'new_rules'],
  hostsRestricted: false,
  ...over,
})

function renderSection(isAdmin = true) {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <OrgChannelsSection orgId="org-1" isAdmin={isAdmin} members={members} />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

async function openEdit() {
  fireEvent.click(await screen.findByRole('button', {name: 'Edit channel'}))
  return screen.getByTestId('channel-form')
}

const lastBody = () => api.save.mock.lastCall?.[1]

beforeEach(() => {
  vi.resetAllMocks()
  api.save.mockResolvedValue(channel())
  api.test.mockResolvedValue(undefined)
  api.options.mockResolvedValue(null)
})

describe('OrgChannelsSection', () => {
  it('pauses a channel without sending a secret: the URL goes back as unchanged', async () => {
    api.list.mockResolvedValue([webhook()])
    renderSection()
    fireEvent.click(await screen.findByRole('button', {name: 'Pause channel'}))
    await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
    expect(api.save).toHaveBeenCalledWith('org-1', {
      id: 'w1',
      name: 'Build hook',
      kind: 'webhook',
      url: 'hooks.example.com/…',
      urlUnchanged: true,
      enabled: false,
    })
    await waitFor(() => expect(api.list).toHaveBeenCalledTimes(2))
  })

  it('lists a channel of a kind a newer server added, which it can test but not pause or edit', async () => {
    api.list.mockResolvedValue([channel({id: 'c2', name: 'Pager', kind: unknownValue('pagerduty')})])
    renderSection()
    expect(await screen.findByText(/pagerduty/)).toBeInTheDocument()
    const pause = screen.getByRole('button', {name: 'Pause channel'})
    expect(pause).toBeDisabled()
    expect(pause).toHaveAttribute('title', "This version of the app can't change this kind of channel.")
    fireEvent.click(pause)
    expect(screen.getByRole('button', {name: 'Edit channel'})).toBeDisabled()
    expect(api.save).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', {name: 'Test channel'}))
    await waitFor(() => expect(api.test).toHaveBeenCalledWith('org-1', 'c2'))
    expect(screen.getByRole('button', {name: 'Remove channel'})).toBeEnabled()
  })

  it('shows which channels sign their deliveries', async () => {
    api.list.mockResolvedValue([
      webhook(),
      webhook({id: 'w2', name: 'Unsigned hook', hasSecret: false}),
      channel({id: 't1', name: 'Teams room', hasSecret: true}),
    ])
    renderSection()
    expect(await screen.findByText('Unsigned hook')).toBeInTheDocument()
    expect(screen.getAllByText('Signed')).toHaveLength(1)
    expect(screen.getByTitle('Deliveries are signed with an HMAC secret')).toBeInTheDocument()
  })

  it('edits a paused channel keeping its URL, secret and paused state', async () => {
    api.list.mockResolvedValue([webhook({enabled: false})])
    renderSection()
    await openEdit()

    const url = screen.getByLabelText('Channel URL')
    expect(url).toHaveValue('')
    expect(url).toHaveAttribute('placeholder', 'hooks.example.com/…')
    expect(url).toHaveAccessibleDescription('Leave empty to keep the current URL.')
    expect(screen.getByLabelText('Signing secret')).toHaveAttribute(
      'placeholder',
      'A signing secret is set (type a new one to replace it)',
    )

    fireEvent.change(screen.getByPlaceholderText(/Channel name/), {target: {value: ' Release hook '}})
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))

    await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
    expect(lastBody()).toEqual({
      id: 'w1',
      name: 'Release hook',
      kind: 'webhook',
      url: 'hooks.example.com/…',
      urlUnchanged: true,
      enabled: false,
    })
    expect(await screen.findByText('Channel updated')).toBeInTheDocument()
  })

  it('sends a new URL and a new secret only when typed', async () => {
    api.list.mockResolvedValue([webhook()])
    renderSection()
    await openEdit()

    fireEvent.change(screen.getByLabelText('Channel URL'), {target: {value: ' https://hooks.example.com/new '}})
    fireEvent.change(screen.getByLabelText('Signing secret'), {target: {value: ' s3cret '}})
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))

    await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
    expect(lastBody()).toEqual({
      id: 'w1',
      name: 'Build hook',
      kind: 'webhook',
      url: 'https://hooks.example.com/new',
      secret: 's3cret',
      enabled: true,
    })
  })

  it('removes the secret on request, and can take the request back', async () => {
    api.list.mockResolvedValue([webhook()])
    renderSection()
    await openEdit()

    fireEvent.change(screen.getByLabelText('Signing secret'), {target: {value: 'typed-then-dropped'}})
    fireEvent.click(screen.getByRole('button', {name: 'Remove secret'}))
    expect(screen.getByText(/will be removed when you save/)).toBeInTheDocument()
    expect(screen.queryByLabelText('Signing secret')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'Keep secret'}))
    expect(screen.getByLabelText('Signing secret')).toHaveValue('')
    fireEvent.click(screen.getByRole('button', {name: 'Remove secret'}))
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))

    await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
    expect(lastBody()).toEqual({
      id: 'w1',
      name: 'Build hook',
      kind: 'webhook',
      url: 'hooks.example.com/…',
      urlUnchanged: true,
      enabled: true,
      clear: ['secret'],
    })
  })

  it('offers no secret on a Teams channel', async () => {
    api.list.mockResolvedValue([channel({hasSecret: true})])
    renderSection()
    await openEdit()

    expect(screen.queryByLabelText('Signing secret')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Remove secret'})).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
    expect(lastBody()).not.toHaveProperty('secret')
    expect(lastBody()).not.toHaveProperty('clear')
  })

  it('works against a server that does not say whether a secret is set', async () => {
    const {hasSecret: _, ...old} = webhook({url: 'https://hooks.example.com/full/path?token=t'})
    api.list.mockResolvedValue([old])
    renderSection()
    expect(await screen.findByText('Build hook')).toBeInTheDocument()
    expect(screen.queryByText('Signed')).not.toBeInTheDocument()
    await openEdit()

    expect(screen.queryByRole('button', {name: 'Remove secret'})).not.toBeInTheDocument()
    expect(screen.getByLabelText('Signing secret')).toHaveAttribute(
      'placeholder',
      'HMAC secret (unchanged unless replaced)',
    )
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
    expect(lastBody()).toMatchObject({
      kind: 'webhook',
      url: 'https://hooks.example.com/full/path?token=t',
      enabled: true,
    })
    expect(lastBody()).not.toHaveProperty('secret')
  })

  it('creates a channel, with a secret only on a kind that signs', async () => {
    api.list.mockResolvedValue([])
    renderSection()
    fireEvent.click(await screen.findByRole('button', {name: 'Add channel'}))

    fireEvent.click(screen.getByRole('button', {name: 'Add'}))
    expect(await screen.findByText('URL is required')).toBeInTheDocument()
    expect(api.save).not.toHaveBeenCalled()

    fireEvent.change(screen.getByPlaceholderText(/Channel name/), {target: {value: 'Room'}})
    fireEvent.change(screen.getByLabelText('Channel URL'), {target: {value: 'https://hooks.example.com/x'}})
    fireEvent.change(screen.getByLabelText('Signing secret'), {target: {value: 'typed-on-webhook'}})
    fireEvent.change(screen.getByLabelText('Channel kind'), {target: {value: 'teams'}})
    expect(screen.queryByLabelText('Signing secret')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Add'}))

    await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
    expect(lastBody()).toEqual({name: 'Room', kind: 'teams', url: 'https://hooks.example.com/x'})
  })

  it('creates a Teams Workflows channel, without a secret, and says what it replaces', async () => {
    api.list.mockResolvedValue([])
    renderSection()
    fireEvent.click(await screen.findByRole('button', {name: 'Add channel'}))

    fireEvent.change(screen.getByLabelText('Channel kind'), {target: {value: 'teams-workflow'}})
    expect(screen.getByRole('option', {name: 'Teams Workflows'})).toBeInTheDocument()
    expect(
      screen.getByText('Teams Workflows (replaces incoming webhooks; deliveries are not signed). HTTPS required.'),
    ).toBeInTheDocument()
    expect(screen.queryByLabelText('Signing secret')).not.toBeInTheDocument()
    const url = 'https://prod-00.westeurope.logic.azure.com:443/workflows/x/triggers/manual/paths/invoke?sig=s'
    fireEvent.change(screen.getByPlaceholderText(/Channel name/), {target: {value: 'Room'}})
    fireEvent.change(screen.getByLabelText('Channel URL'), {target: {value: url}})
    fireEvent.click(screen.getByRole('button', {name: 'Add'}))

    await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
    expect(lastBody()).toEqual({name: 'Room', kind: 'teams-workflow', url})
  })

  it('lists a Teams Workflows channel as a kind it knows: labelled, editable, unsigned', async () => {
    api.list.mockResolvedValue([channel({kind: 'teams-workflow', url: 'prod-00.westeurope.logic.azure.com:443/…'})])
    renderSection()
    expect(await screen.findByText(/Teams Workflows/)).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Pause channel'})).toBeEnabled()
    await openEdit()

    expect(screen.queryByLabelText('Signing secret')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))
    await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
    expect(lastBody()).toEqual({
      id: 'c1',
      name: 'Ops hook',
      kind: 'teams-workflow',
      url: 'prod-00.westeurope.logic.azure.com:443/…',
      urlUnchanged: true,
      enabled: true,
    })
  })

  it('shows a save the server refuses at the field it names', async () => {
    api.list.mockResolvedValue([webhook()])
    api.save.mockRejectedValue(new ApiError('url must use https', 400, 'INVALID_CHANNEL', null, {field: 'url'}))
    renderSection()
    await openEdit()

    fireEvent.change(screen.getByLabelText('Channel URL'), {target: {value: 'http://hooks.example.com/x'}})
    fireEvent.click(screen.getByRole('button', {name: 'Save'}))

    const url = screen.getByLabelText('Channel URL')
    await waitFor(() => expect(url).toHaveAttribute('aria-invalid', 'true'))
    expect(url).toHaveAccessibleDescription(/url must use https/)
    expect(url).toHaveValue('http://hooks.example.com/x')
    fireEvent.change(url, {target: {value: 'https://hooks.example.com/x'}})
    expect(url).not.toHaveAttribute('aria-invalid')
  })

  describe('test delivery', () => {
    it.each([
      [
        'the endpoint status',
        new ApiError('internal server error', 502, 'CHANNEL_DELIVERY_FAILED', null, {
          reason: 'the endpoint answered HTTP 404',
          status: 404,
        }),
        'The endpoint answered HTTP 404.',
      ],
      [
        'the reason, as the server masks a 5xx message',
        new ApiError('internal server error', 502, 'CHANNEL_DELIVERY_FAILED', null, {
          reason: 'the connection timed out',
        }),
        'The connection timed out',
      ],
      [
        'busy deliveries',
        new ApiError('internal server error', 503, 'CHANNEL_DELIVERIES_BUSY'),
        'Deliveries are busy right now. Try the test again shortly.',
      ],
      [
        'its rate limit',
        new ApiError('Too many requests', 429, 'RATE_LIMITED'),
        'Too many test deliveries. Wait a minute and try again.',
      ],
      ['an older server’s answer as sent', new ApiError('internal server error', 502), 'internal server error'],
    ])('explains a failure by %s', async (_case, err, text) => {
      api.list.mockResolvedValue([webhook()])
      api.test.mockRejectedValue(err)
      renderSection()
      fireEvent.click(await screen.findByRole('button', {name: 'Test channel'}))

      expect(await screen.findByText('Test delivery failed')).toBeInTheDocument()
      expect(screen.getByText(text)).toBeInTheDocument()
    })
  })
  describe('Jira and email channels, events and the deployment options', () => {
    it('offers only the kinds a server without the options route has', async () => {
      api.list.mockResolvedValue([])
      renderSection()
      fireEvent.click(await screen.findByRole('button', {name: 'Add channel'}))
      expect(screen.queryByRole('option', {name: 'Jira'})).not.toBeInTheDocument()
      expect(screen.queryByRole('option', {name: 'Email'})).not.toBeInTheDocument()
    })

    it('offers the kinds the server delivers: no email without its mail settings', async () => {
      api.list.mockResolvedValue([])
      api.options.mockResolvedValue(fullOptions({kinds: ['jira', 'slack', 'teams', 'teams-workflow', 'webhook']}))
      renderSection()
      fireEvent.click(await screen.findByRole('button', {name: 'Add channel'}))
      expect(await screen.findByRole('option', {name: 'Jira'})).toBeInTheDocument()
      expect(screen.queryByRole('option', {name: 'Email'})).not.toBeInTheDocument()
    })

    it('creates a Jira channel with its account email, API token and project key', async () => {
      api.list.mockResolvedValue([])
      api.options.mockResolvedValue(fullOptions())
      renderSection()
      fireEvent.click(await screen.findByRole('button', {name: 'Add channel'}))
      await screen.findByRole('option', {name: 'Jira'})
      fireEvent.change(screen.getByLabelText('Channel kind'), {target: {value: 'jira'}})
      expect(screen.queryByLabelText('Signing secret')).not.toBeInTheDocument()
      fireEvent.change(screen.getByPlaceholderText(/Channel name/), {target: {value: 'Tickets'}})
      fireEvent.change(screen.getByLabelText('Channel URL'), {target: {value: 'https://acme.atlassian.net'}})
      fireEvent.click(screen.getByRole('button', {name: 'Add'}))
      expect(await screen.findAllByText('Required')).toHaveLength(3)
      expect(api.save).not.toHaveBeenCalled()

      fireEvent.change(screen.getByLabelText('Jira account email'), {target: {value: 'bot@acme.io'}})
      fireEvent.change(screen.getByLabelText('Jira API token'), {target: {value: 'tok'}})
      fireEvent.change(screen.getByLabelText('Jira project key'), {target: {value: 'PAD'}})
      fireEvent.click(screen.getByRole('button', {name: 'Add'}))
      await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
      expect(lastBody()).toEqual({
        name: 'Tickets',
        kind: 'jira',
        url: 'https://acme.atlassian.net',
        email: 'bot@acme.io',
        token: 'tok',
        project: 'PAD',
      })
    })

    it('creates an email channel for chosen members, with no URL, and the events chosen', async () => {
      api.list.mockResolvedValue([])
      api.options.mockResolvedValue(fullOptions())
      renderSection()
      fireEvent.click(await screen.findByRole('button', {name: 'Add channel'}))
      await screen.findByRole('option', {name: 'Email'})
      fireEvent.change(screen.getByLabelText('Channel kind'), {target: {value: 'email'}})
      expect(screen.queryByLabelText('Channel URL')).not.toBeInTheDocument()
      fireEvent.click(screen.getByRole('button', {name: 'Add'}))
      expect(await screen.findByText('Choose at least one recipient')).toBeInTheDocument()

      fireEvent.click(screen.getByLabelText('Ann'))
      fireEvent.click(screen.getByLabelText('bob@acme.io'))
      fireEvent.click(screen.getByLabelText('Health regressions'))
      fireEvent.click(screen.getByRole('button', {name: 'Add'}))
      await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
      expect(lastBody()).toEqual({name: '', kind: 'email', recipients: ['ann', 'bob'], events: ['drift', 'new_rules']})
    })

    it('shows a recipient the server refuses at the recipients', async () => {
      api.list.mockResolvedValue([])
      api.options.mockResolvedValue(fullOptions())
      api.save.mockRejectedValue(
        new ApiError('recipient "bob" is not a member', 400, 'INVALID_CHANNEL', null, {field: 'recipients'}),
      )
      renderSection()
      fireEvent.click(await screen.findByRole('button', {name: 'Add channel'}))
      await screen.findByRole('option', {name: 'Email'})
      fireEvent.change(screen.getByLabelText('Channel kind'), {target: {value: 'email'}})
      fireEvent.click(screen.getByLabelText('Ann'))
      fireEvent.click(screen.getByRole('button', {name: 'Add'}))
      expect(await screen.findByText('recipient "bob" is not a member')).toBeInTheDocument()
      expect(screen.getByRole('group', {name: 'Recipients'})).toHaveAttribute('aria-invalid', 'true')
    })

    it('lists the recipients by name, a Jira project and the events a channel chose', async () => {
      api.list.mockResolvedValue([
        channel({id: 'e1', kind: 'email', url: '', recipients: ['ann', 'gone'], events: ['new_rules']}),
        channel({id: 'j1', kind: 'jira', url: 'acme.atlassian.net/…', project: 'PAD', hasToken: true}),
      ])
      renderSection()
      expect(await screen.findByText('to Ann, a former member')).toBeInTheDocument()
      expect(screen.getByText('· only new rules')).toBeInTheDocument()
      expect(screen.getByText('· project PAD')).toBeInTheDocument()
    })

    it('edits an email channel keeping its events, and pauses it without a URL', async () => {
      const mail = channel({id: 'e1', kind: 'email', url: '', recipients: ['ann'], events: ['drift']})
      api.list.mockResolvedValue([mail])
      renderSection()
      fireEvent.click(await screen.findByRole('button', {name: 'Pause channel'}))
      await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
      expect(lastBody()).toEqual({id: 'e1', name: 'Ops hook', kind: 'email', enabled: false})

      await openEdit()
      expect(screen.getByLabelText('Drift: findings a baseline does not have')).toBeChecked()
      expect(screen.getByLabelText('New rules that report findings')).not.toBeChecked()
      fireEvent.click(screen.getByLabelText('bob@acme.io'))
      fireEvent.click(screen.getByRole('button', {name: 'Save'}))
      await waitFor(() => expect(api.save).toHaveBeenCalledTimes(2))
      expect(lastBody()).toEqual({id: 'e1', name: 'Ops hook', kind: 'email', enabled: true, recipients: ['ann', 'bob']})
    })

    it('says a host list applies, and shows it when the server does', async () => {
      api.list.mockResolvedValue([])
      api.options.mockResolvedValue(fullOptions({hostsRestricted: true}))
      const {unmount} = renderSection()
      fireEvent.click(await screen.findByRole('button', {name: 'Add channel'}))
      await waitFor(() =>
        expect(screen.getByLabelText('Channel URL')).toHaveAccessibleDescription(
          /delivers only to hosts its operator allows/,
        ),
      )
      unmount()

      api.options.mockResolvedValue(
        fullOptions({hostsRestricted: true, allowedHosts: ['hooks.slack.com', '*.logic.azure.com']}),
      )
      renderSection()
      fireEvent.click(await screen.findByRole('button', {name: 'Add channel'}))
      expect(await screen.findByText('Allowed hosts: hooks.slack.com, *.logic.azure.com')).toBeInTheDocument()
    })

    it('lists an unreadable channel and asks for its URL again', async () => {
      api.list.mockResolvedValue([webhook({url: '', unreadable: true, hasSecret: false})])
      renderSection()
      expect(await screen.findByText('URL unreadable')).toBeInTheDocument()
      const pause = screen.getByRole('button', {name: 'Pause channel'})
      expect(pause).toBeDisabled()
      expect(pause).toHaveAttribute('title', expect.stringMatching(/encryption key changed/))

      await openEdit()
      fireEvent.click(screen.getByRole('button', {name: 'Save'}))
      const url = screen.getByLabelText('Channel URL')
      await waitFor(() => expect(url).toHaveAttribute('aria-invalid', 'true'))
      expect(url).toHaveAccessibleDescription(/be decrypted/)
      expect(api.save).not.toHaveBeenCalled()

      fireEvent.change(url, {target: {value: 'https://hooks.example.com/again'}})
      fireEvent.click(screen.getByRole('button', {name: 'Save'}))
      await waitFor(() => expect(api.save).toHaveBeenCalledTimes(1))
      expect(lastBody()).toMatchObject({id: 'w1', url: 'https://hooks.example.com/again'})
      expect(lastBody()).not.toHaveProperty('urlUnchanged')
    })
  })
})
