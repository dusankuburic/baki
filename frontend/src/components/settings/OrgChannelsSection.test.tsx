import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {channelsApi, type OrgChannel} from '@/api/governance'
import {unknownValue} from '@/testing/testHelpers'
import OrgChannelsSection from './OrgChannelsSection'

vi.mock('@/api/governance', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/governance')>()),
  channelsApi: {list: vi.fn(), save: vi.fn(), remove: vi.fn(), test: vi.fn()},
}))

const api = vi.mocked(channelsApi)

const channel = (over: Partial<OrgChannel> = {}): OrgChannel => ({
  id: 'c1',
  name: 'Ops hook',
  kind: 'teams',
  url: 'https://hooks.example.com/ops',
  enabled: true,
  createdAt: '2026-01-01T00:00:00Z',
  ...over,
})

function renderSection() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <OrgChannelsSection orgId="org-1" isAdmin />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.resetAllMocks()
  api.save.mockResolvedValue(channel())
  api.test.mockResolvedValue(undefined)
})

describe('OrgChannelsSection', () => {
  it('pauses and edits a channel of a kind it knows', async () => {
    api.list.mockResolvedValue([channel()])
    renderSection()
    expect(await screen.findByText(/Teams/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Pause channel'}))
    await waitFor(() =>
      expect(api.save).toHaveBeenCalledWith(
        'org-1',
        expect.objectContaining({id: 'c1', kind: 'teams', enabled: false}),
      ),
    )
    expect(screen.getByRole('button', {name: 'Edit channel'})).toBeEnabled()
  })

  // Tolerant wire enums: a kind a newer server added is listed as sent and
  // can be tested and removed, but pausing or editing would save the kind
  // back, and this app sends only the kinds it knows.
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
})
