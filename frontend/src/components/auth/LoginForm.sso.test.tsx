import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'

const ssoInfoMock = vi.fn()
const ssoExchangeMock = vi.fn()

vi.mock('@/api/auth', async (importOriginal) => {
  const mod = await importOriginal<typeof import('@/api/auth')>()
  return {
    ...mod,
    authApi: {
      ...mod.authApi,
      ssoInfo: (...args: unknown[]) => ssoInfoMock(...args),
      ssoExchange: (...args: unknown[]) => ssoExchangeMock(...args),
    },
  }
})

vi.mock('@/api/client', () => ({
  request: vi.fn(),
  getBackendConfig: vi.fn().mockResolvedValue({ apiUrl: 'http://api.test' }),
  registerRefreshCallback: vi.fn(),
  invalidateConfigCache: vi.fn(),
  setSessionToken: vi.fn(),
}))

import LoginForm from './LoginForm'
import { useAuthStore } from '@/stores/authStore'

const initialState = useAuthStore.getState()

function setHash(hash: string) {
  window.history.replaceState(null, '', '/' + hash)
}

beforeEach(() => {
  useAuthStore.setState(initialState, true)
  ssoInfoMock.mockReset()
  ssoExchangeMock.mockReset()
  setHash('')
})

describe('LoginForm SSO', () => {
  it('hides the SSO button when SSO is not configured', async () => {
    ssoInfoMock.mockResolvedValue({ enabled: false })
    render(<LoginForm />)
    await waitFor(() => expect(ssoInfoMock).toHaveBeenCalled())
    expect(screen.queryByText(/continue with/i)).not.toBeInTheDocument()
  })

  it('shows the provider button when SSO is enabled', async () => {
    ssoInfoMock.mockResolvedValue({ enabled: true, provider: 'Microsoft' })
    render(<LoginForm />)
    expect(await screen.findByText('Continue with Microsoft')).toBeInTheDocument()
  })

  it('exchanges an ssoTicket from the URL fragment on mount', async () => {
    ssoInfoMock.mockResolvedValue({ enabled: true, provider: 'Microsoft' })
    const ticketMock = vi.fn().mockResolvedValue(undefined)
    useAuthStore.setState({ ...initialState, loginWithSSOTicket: ticketMock }, true)
    setHash('#ssoTicket=ticket-abc')

    render(<LoginForm />)

    await waitFor(() => expect(ticketMock).toHaveBeenCalledWith('ticket-abc'))
    // The single-use ticket must be stripped from the address bar.
    expect(window.location.hash).toBe('')
  })

  it('surfaces an ssoError from the URL fragment', async () => {
    ssoInfoMock.mockResolvedValue({ enabled: true, provider: 'Microsoft' })
    setHash('#ssoError=' + encodeURIComponent('identity verification failed'))

    render(<LoginForm />)

    expect(await screen.findByText('identity verification failed')).toBeInTheDocument()
    expect(window.location.hash).toBe('')
  })

  it('clicking the SSO button navigates to the backend start endpoint', async () => {
    ssoInfoMock.mockResolvedValue({ enabled: true, provider: 'Microsoft' })
    const hrefSpy = vi.fn()
    const original = window.location
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: {
        ...original,
        get href() { return original.href },
        set href(v: string) { hrefSpy(v) },
        hash: '',
        pathname: '/',
        search: '',
      },
    })

    try {
      render(<LoginForm />)
      fireEvent.click(await screen.findByText('Continue with Microsoft'))
      await waitFor(() =>
        expect(hrefSpy).toHaveBeenCalledWith('http://api.test/api/auth/sso/start'),
      )
    } finally {
      Object.defineProperty(window, 'location', { configurable: true, value: original })
    }
  })
})
