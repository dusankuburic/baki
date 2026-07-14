import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'

const resetPasswordMock = vi.fn()

vi.mock('@/api/auth', async importOriginal => {
  const mod = await importOriginal<typeof import('@/api/auth')>()
  return {
    ...mod,
    authApi: {...mod.authApi, resetPassword: (...a: unknown[]) => resetPasswordMock(...a)},
  }
})

import ResetPasswordView from './ResetPasswordView'

beforeEach(() => vi.resetAllMocks())

describe('ResetPasswordView', () => {
  it('blocks submit when passwords do not match', async () => {
    render(<ResetPasswordView token="tok" onDone={() => {}} />)
    fireEvent.change(screen.getByPlaceholderText('New password'), {target: {value: 'LongEnoughPass1!'}})
    fireEvent.change(screen.getByPlaceholderText('Confirm new password'), {target: {value: 'Different1!'}})
    fireEvent.click(screen.getByRole('button', {name: /reset password/i}))
    expect(await screen.findByText(/do not match/i)).toBeInTheDocument()
    expect(resetPasswordMock).not.toHaveBeenCalled()
  })

  it('rejects passwords shorter than 12 characters without calling the API', async () => {
    render(<ResetPasswordView token="tok" onDone={() => {}} />)
    fireEvent.change(screen.getByPlaceholderText('New password'), {target: {value: 'short1!'}})
    fireEvent.change(screen.getByPlaceholderText('Confirm new password'), {target: {value: 'short1!'}})
    fireEvent.click(screen.getByRole('button', {name: /reset password/i}))
    expect(await screen.findByText(/at least 12 characters/i)).toBeInTheDocument()
    expect(resetPasswordMock).not.toHaveBeenCalled()
  })

  it('submits the token and new password, then shows success', async () => {
    resetPasswordMock.mockResolvedValue({status: 'ok'})
    render(<ResetPasswordView token="my-token" onDone={() => {}} />)
    fireEvent.change(screen.getByPlaceholderText('New password'), {target: {value: 'BrandNewPassw0rd!'}})
    fireEvent.change(screen.getByPlaceholderText('Confirm new password'), {target: {value: 'BrandNewPassw0rd!'}})
    fireEvent.click(screen.getByRole('button', {name: /reset password/i}))

    await waitFor(() => expect(resetPasswordMock).toHaveBeenCalledWith('my-token', 'BrandNewPassw0rd!'))
    expect(await screen.findByText(/password updated/i)).toBeInTheDocument()
  })

  it('surfaces the API error message on failure', async () => {
    resetPasswordMock.mockRejectedValue(new Error('invalid or expired reset token'))
    render(<ResetPasswordView token="bad" onDone={() => {}} />)
    fireEvent.change(screen.getByPlaceholderText('New password'), {target: {value: 'BrandNewPassw0rd!'}})
    fireEvent.change(screen.getByPlaceholderText('Confirm new password'), {target: {value: 'BrandNewPassw0rd!'}})
    fireEvent.click(screen.getByRole('button', {name: /reset password/i}))
    expect(await screen.findByText(/invalid or expired/i)).toBeInTheDocument()
  })
})
