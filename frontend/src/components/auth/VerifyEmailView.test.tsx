import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen} from '@testing-library/react'

const verifyEmailMock = vi.fn()

vi.mock('@/api/auth', async importOriginal => {
  const mod = await importOriginal<typeof import('@/api/auth')>()
  return {
    ...mod,
    authApi: {...mod.authApi, verifyEmail: (...a: unknown[]) => verifyEmailMock(...a)},
  }
})

import VerifyEmailView from './VerifyEmailView'

beforeEach(() => vi.resetAllMocks())

describe('VerifyEmailView', () => {
  it('redeems the token on mount and shows success', async () => {
    verifyEmailMock.mockResolvedValue({status: 'ok'})
    render(<VerifyEmailView token="vt" onDone={() => {}} />)
    expect(await screen.findByText(/email verified/i)).toBeInTheDocument()
    expect(verifyEmailMock).toHaveBeenCalledWith('vt')
  })

  it('shows an error state when verification fails', async () => {
    verifyEmailMock.mockRejectedValue(new Error('invalid or expired verification token'))
    render(<VerifyEmailView token="bad" onDone={() => {}} />)
    expect(await screen.findByText(/verification failed/i)).toBeInTheDocument()
    expect(await screen.findByText(/invalid or expired/i)).toBeInTheDocument()
  })
})
