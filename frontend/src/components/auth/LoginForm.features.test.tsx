import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, waitFor} from '@testing-library/react'

const featuresMock = vi.fn()

vi.mock('@/api/system', () => ({
  systemApi: {features: (...args: unknown[]) => featuresMock(...args)},
}))

vi.mock('@/api/auth', async importOriginal => {
  const mod = await importOriginal<typeof import('@/api/auth')>()
  return {...mod, authApi: {...mod.authApi, ssoInfo: vi.fn().mockResolvedValue({enabled: false})}}
})

import LoginForm from './LoginForm'
import {useAuthStore} from '@/stores/authStore'

const initialState = useAuthStore.getState()

beforeEach(() => {
  useAuthStore.setState(initialState, true)
  featuresMock.mockReset()
})

describe('LoginForm sign-up flag', () => {
  it('hides the register toggle when sign-up is disabled', async () => {
    featuresMock.mockResolvedValue({disableSignUp: true})
    render(<LoginForm />)
    await waitFor(() => expect(featuresMock).toHaveBeenCalled())
    await waitFor(() => expect(screen.queryByRole('button', {name: 'Sign up'})).toBeNull())
  })

  it('keeps the register toggle when sign-up is open', async () => {
    featuresMock.mockResolvedValue({disableSignUp: false})
    render(<LoginForm />)
    await waitFor(() => expect(featuresMock).toHaveBeenCalled())
    expect(screen.getByRole('button', {name: 'Sign up'})).toBeInTheDocument()
  })

  it('keeps the register toggle when the flags cannot be read', async () => {
    featuresMock.mockRejectedValue(new Error('offline'))
    render(<LoginForm />)
    await waitFor(() => expect(featuresMock).toHaveBeenCalled())
    expect(screen.getByRole('button', {name: 'Sign up'})).toBeInTheDocument()
  })
})
