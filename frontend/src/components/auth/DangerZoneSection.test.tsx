import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {DangerZoneSection} from './DangerZoneSection'
import {useAuthStore} from '@/stores/authStore'
import {authApi} from '@/api/auth'

vi.mock('@/api/auth', () => ({
  authApi: {
    deleteAccount: vi.fn(),
    logout: vi.fn().mockResolvedValue(undefined),
  },
}))

const logout = vi.fn().mockResolvedValue(undefined)

beforeEach(() => {
  vi.clearAllMocks()
  sessionStorage.clear()
  useAuthStore.setState({user: {id: 'u1', email: 'Ada@Example.com', role: 'member'}, logout})
})

describe('DangerZoneSection', () => {
  it('keeps the confirmation folded and arms deletion only once the email matches', async () => {
    vi.mocked(authApi.deleteAccount).mockResolvedValue({status: 'ok'})
    render(<DangerZoneSection />)
    expect(screen.queryByLabelText(/To confirm, type your email address/)).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'Delete account…'}))
    const field = screen.getByLabelText('To confirm, type your email address (Ada@Example.com).')
    const submit = screen.getByRole('button', {name: 'Permanently delete my account'})
    expect(submit).toBeDisabled()

    fireEvent.change(field, {target: {value: 'ada@example.co'}})
    expect(submit).toBeDisabled()
    fireEvent.change(field, {target: {value: ' ada@example.com '}})
    expect(submit).toBeEnabled()

    fireEvent.click(submit)
    await waitFor(() => expect(authApi.deleteAccount).toHaveBeenCalledWith('ada@example.com'))
    await waitFor(() => expect(logout).toHaveBeenCalledTimes(1))
    expect(sessionStorage.getItem('auth_notice')).toBe('account-deleted')
  })

  it('shows the server error inline and keeps the session', async () => {
    vi.mocked(authApi.deleteAccount).mockRejectedValue(new Error('account has active organisations'))
    render(<DangerZoneSection />)
    fireEvent.click(screen.getByRole('button', {name: 'Delete account…'}))
    fireEvent.change(screen.getByLabelText(/To confirm/), {target: {value: 'ada@example.com'}})
    fireEvent.click(screen.getByRole('button', {name: 'Permanently delete my account'}))
    expect(await screen.findByRole('alert')).toHaveTextContent('account has active organisations')
    expect(logout).not.toHaveBeenCalled()
  })

  it('Cancel folds the form and clears what was typed', () => {
    render(<DangerZoneSection />)
    fireEvent.click(screen.getByRole('button', {name: 'Delete account…'}))
    fireEvent.change(screen.getByLabelText(/To confirm/), {target: {value: 'ada@example.com'}})
    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
    expect(screen.queryByLabelText(/To confirm/)).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Delete account…'}))
    expect(screen.getByLabelText(/To confirm/)).toHaveValue('')
  })
})
