import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent, act} from '@testing-library/react'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({openURL: vi.fn().mockResolvedValue(undefined)}),
}))

import DeviceAuthLoginButton, {type DeviceAuthProvider} from './DeviceAuthLoginButton'

function makeProvider(): DeviceAuthProvider & {
  startAuth: ReturnType<typeof vi.fn>
  pollAuth: ReturnType<typeof vi.fn>
} {
  return {
    name: 'Copilot',
    label: 'Sign in with GitHub',
    buttonClassName: '',
    getUser: vi.fn().mockResolvedValue(null),
    startAuth: vi
      .fn()
      .mockResolvedValueOnce({
        device_code: 'old',
        user_code: 'OLD-1',
        verification_uri: 'https://x',
        interval: 5,
        expires_in: 900,
      })
      .mockResolvedValueOnce({
        device_code: 'new',
        user_code: 'NEW-2',
        verification_uri: 'https://x',
        interval: 5,
        expires_in: 900,
      }),
    // The abandoned code expires; the new one is still pending.
    pollAuth: vi.fn(async (code: string) =>
      code === 'old' ? {status: 'error', error: 'expired_token'} : {status: 'pending'},
    ),
    revokeAuth: vi.fn(),
  } as never
}

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('DeviceAuthLoginButton', () => {
  it('a cancelled attempt cannot kill the next sign-in', async () => {
    const provider = makeProvider()
    render(<DeviceAuthLoginButton provider={provider} />)
    await act(async () => {})

    await act(async () => {
      fireEvent.click(screen.getByRole('button', {name: /sign in with github/i}))
    })
    expect(screen.getByText('OLD-1')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
    await act(async () => {
      fireEvent.click(screen.getByRole('button', {name: /sign in with github/i}))
    })
    expect(screen.getByText('NEW-2')).toBeInTheDocument()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(11_000)
    })

    // Only the live code is polled; the old loop stayed dead.
    expect(provider.pollAuth).not.toHaveBeenCalledWith('old')
    expect(provider.pollAuth).toHaveBeenCalledWith('new')
    expect(screen.getByText('NEW-2')).toBeInTheDocument()
    expect(screen.queryByText('expired_token')).not.toBeInTheDocument()
  })

  it('stops polling and says so once the code expires', async () => {
    const provider = makeProvider()
    provider.startAuth.mockReset().mockResolvedValue({
      device_code: 'new',
      user_code: 'NEW-2',
      verification_uri: 'https://x',
      interval: 5,
      expires_in: 12,
    })
    render(<DeviceAuthLoginButton provider={provider} />)
    await act(async () => {})
    await act(async () => {
      fireEvent.click(screen.getByRole('button', {name: /sign in with github/i}))
    })

    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000)
    })
    expect(screen.getByText(/code expired/i)).toBeInTheDocument()
    const calls = provider.pollAuth.mock.calls.length
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    expect(provider.pollAuth.mock.calls.length).toBe(calls)
  })
})
