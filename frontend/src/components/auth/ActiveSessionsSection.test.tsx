import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, within} from '@testing-library/react'
import {ActiveSessionsSection} from './ActiveSessionsSection'
import {ToastProvider, ConfirmProvider} from '@/components/shared'
import {authApi} from '@/api/auth'

vi.mock('@/api/auth', () => ({
  authApi: {
    listSessions: vi.fn(),
    revokeSession: vi.fn(),
  },
}))

vi.mock('@/stores/authStore', () => ({
  getCurrentSessionId: () => 'current-jti',
}))

const EDGE =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 Edg/128.0.0.0'
const CHROME =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36'
const IPHONE =
  'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1'

const HOUR = 3600e3
function sessions() {
  const now = Date.now()
  const iso = (t: number) => new Date(t).toISOString()
  return [
    // Older than the current one: sorting must still pin "this device" first.
    {
      id: 'other-1',
      createdAt: iso(now - 30 * HOUR),
      expiresAt: iso(now + 18 * HOUR),
      userAgent: CHROME,
      ip: '10.0.0.2',
    },
    {
      id: 'current-jti',
      createdAt: iso(now - 2 * HOUR),
      expiresAt: iso(now + 22 * HOUR),
      userAgent: EDGE,
      ip: '10.0.0.1',
    },
    {id: 'other-2', createdAt: iso(now - 5 * HOUR), expiresAt: iso(now + 19 * HOUR), userAgent: IPHONE},
    {id: 'other-3', createdAt: iso(now - 50 * HOUR), expiresAt: iso(now + 1 * HOUR), userAgent: CHROME, ip: '10.0.0.3'},
    {id: 'other-4', createdAt: iso(now - 60 * HOUR), expiresAt: iso(now + 2 * HOUR)},
    {id: 'other-5', createdAt: iso(now - 70 * HOUR), expiresAt: iso(now + 3 * HOUR), userAgent: CHROME},
    {id: 'other-6', createdAt: iso(now - 80 * HOUR), expiresAt: iso(now + 4 * HOUR), userAgent: CHROME},
  ]
}

function renderSection() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <ActiveSessionsSection />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  vi.mocked(authApi.listSessions).mockResolvedValue(sessions())
  vi.mocked(authApi.revokeSession).mockResolvedValue(undefined)
})

describe('ActiveSessionsSection', () => {
  it('pins this device first without a sign-out control, then the newest sign-ins', async () => {
    renderSection()
    const rows = await screen.findAllByRole('listitem')
    expect(rows).toHaveLength(5)
    expect(rows[0]).toHaveTextContent('Edge on Windows')
    expect(rows[0]).toHaveTextContent('This device')
    expect(rows[0]).toHaveTextContent(/Signed in 2 hours ago from 10\.0\.0\.1/)
    expect(rows[0]).toHaveTextContent(/Expires in 22 hours/)
    expect(within(rows[0]).queryByRole('button')).toBeNull()
    expect(rows[1]).toHaveTextContent('Safari on iOS')
    expect(rows[1]).toHaveTextContent(/Signed in 5 hours agoExpires/)
    expect(within(rows[1]).getByRole('button', {name: 'Sign out Safari on iOS'})).toBeInTheDocument()
    expect(rows[4]).toHaveTextContent('Unknown device')
  })

  it('shows five rows and unfolds the rest on request', async () => {
    renderSection()
    await screen.findAllByRole('listitem')
    const toggle = screen.getByRole('button', {name: 'Show all 7 sessions'})
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    fireEvent.click(toggle)
    expect(screen.getAllByRole('listitem')).toHaveLength(7)
    fireEvent.click(screen.getByRole('button', {name: 'Show fewer'}))
    expect(screen.getAllByRole('listitem')).toHaveLength(5)
  })

  it('signs out one device after confirming, then drops the row and toasts', async () => {
    renderSection()
    await screen.findAllByRole('listitem')
    fireEvent.click(screen.getByRole('button', {name: 'Sign out Safari on iOS'}))
    expect(screen.getByRole('dialog')).toHaveTextContent('Sign out Safari on iOS?')
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', {name: 'Sign out'}))
    await waitFor(() => expect(authApi.revokeSession).toHaveBeenCalledWith('other-2'))
    await waitFor(() => expect(screen.queryByText('Safari on iOS')).not.toBeInTheDocument())
    expect(await screen.findByText('Safari on iOS signed out')).toBeInTheDocument()
  })

  it('a cancelled confirmation revokes nothing', async () => {
    renderSection()
    await screen.findAllByRole('listitem')
    fireEvent.click(screen.getByRole('button', {name: 'Sign out Safari on iOS'}))
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', {name: 'Cancel'}))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(authApi.revokeSession).not.toHaveBeenCalled()
  })

  it('"Sign out other sessions" confirms with the count and keeps this device', async () => {
    renderSection()
    await screen.findAllByRole('listitem')
    fireEvent.click(screen.getByRole('button', {name: 'Sign out other sessions'}))
    expect(screen.getByRole('dialog')).toHaveTextContent('This signs out 6 other devices. You stay signed in here.')
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', {name: 'Sign out others'}))
    await waitFor(() => expect(authApi.revokeSession).toHaveBeenCalledTimes(6))
    expect(authApi.revokeSession).not.toHaveBeenCalledWith('current-jti')
    await waitFor(() => expect(screen.getAllByRole('listitem')).toHaveLength(1))
    expect(await screen.findByText('Signed out 6 other sessions')).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Sign out other sessions'})).not.toBeInTheDocument()
  })

  it('offers a retry when the list fails to load', async () => {
    vi.mocked(authApi.listSessions).mockRejectedValueOnce(new Error('network down'))
    renderSection()
    expect(await screen.findByRole('alert')).toHaveTextContent('network down')
    expect(screen.queryByText('No active sessions found.')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    expect(await screen.findAllByRole('listitem')).toHaveLength(5)
    expect(authApi.listSessions).toHaveBeenCalledTimes(2)
  })
})
