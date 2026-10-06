import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ChangePasswordSection} from './ChangePasswordSection'
import {ToastProvider} from '@/components/shared'
import {useAuthStore} from '@/stores/authStore'
import {authApi} from '@/api/auth'

vi.mock('@/api/auth', () => ({
  authApi: {
    changePassword: vi.fn(),
    logout: vi.fn().mockResolvedValue(undefined),
  },
}))

const logout = vi.fn().mockResolvedValue(undefined)

function renderSection() {
  return render(
    <ToastProvider>
      <ChangePasswordSection />
    </ToastProvider>,
  )
}

function fill(current: string, next: string, confirm: string) {
  fireEvent.change(screen.getByLabelText('Current password'), {target: {value: current}})
  fireEvent.change(screen.getByLabelText('New password'), {target: {value: next}})
  fireEvent.change(screen.getByLabelText('Confirm new password'), {target: {value: confirm}})
}

const metRules = () => document.querySelectorAll('#change-password-rules li[data-met]').length

beforeEach(() => {
  vi.clearAllMocks()
  sessionStorage.clear()
  useAuthStore.setState({logout})
})

describe('ChangePasswordSection', () => {
  it('shows the server rules while typing and arms the button only when they pass', () => {
    renderSection()
    const submit = screen.getByRole('button', {name: 'Update password'})
    expect(submit).toBeDisabled()
    expect(
      screen.getByText('Changing your password signs you out of every device, including this one.'),
    ).toBeInTheDocument()

    fill('old-password', 'abc', 'abc')
    expect(metRules()).toBe(0)
    fill('old-password', 'correct-horse-battery', 'correct-horse-battery')
    expect(metRules()).toBe(1) // long enough, but only two character classes
    expect(submit).toBeDisabled()

    fill('old-password', 'Correct-horse-1', 'Correct-horse-1')
    expect(metRules()).toBe(2)
    expect(submit).toBeEnabled()

    fill('', 'Correct-horse-1', 'Correct-horse-1')
    expect(submit).toBeDisabled()
  })

  it('flags a mismatched confirmation on blur-sm and disarms the button', () => {
    renderSection()
    fill('old-password', 'Correct-horse-1', 'Correct-horse-2')
    fireEvent.blur(screen.getByLabelText('Confirm new password'))
    expect(screen.getByText('New passwords do not match')).toBeInTheDocument()
    expect(screen.getByLabelText('Confirm new password')).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByRole('button', {name: 'Update password'})).toBeDisabled()
  })

  it('rejects a password over 72 bytes inline', () => {
    renderSection()
    fill('old-password', 'Aa1'.repeat(25), 'Aa1'.repeat(25))
    expect(screen.getByText('At most 72 characters')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Update password'})).toBeDisabled()
  })

  it('reveals a field on its own toggle', () => {
    renderSection()
    const field = screen.getByLabelText('New password')
    expect(field).toHaveAttribute('type', 'password')
    const toggles = screen.getAllByRole('button', {name: 'Show password'})
    fireEvent.click(toggles[1]!)
    expect(field).toHaveAttribute('type', 'text')
    expect(screen.getByRole('button', {name: 'Hide password'})).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByLabelText('Current password')).toHaveAttribute('type', 'password')
  })

  it('changes the password, explains the sign-out and signs out', async () => {
    vi.mocked(authApi.changePassword).mockResolvedValue(undefined)
    renderSection()
    fill('old-password', 'Correct-horse-1', 'Correct-horse-1')
    fireEvent.click(screen.getByRole('button', {name: 'Update password'}))
    await waitFor(() => expect(authApi.changePassword).toHaveBeenCalledWith('old-password', 'Correct-horse-1'))
    await waitFor(() => expect(logout).toHaveBeenCalledTimes(1))
    // The explanation survives the sign-out: LoginForm reads it on mount.
    expect(sessionStorage.getItem('auth_notice')).toBe('password-changed')
  })

  it('keeps the session and shows the server error inline when the change fails', async () => {
    vi.mocked(authApi.changePassword).mockRejectedValue(new Error('invalid old password'))
    renderSection()
    fill('wrong', 'Correct-horse-1', 'Correct-horse-1')
    fireEvent.click(screen.getByRole('button', {name: 'Update password'}))
    expect(await screen.findByRole('alert')).toHaveTextContent('invalid old password')
    expect(logout).not.toHaveBeenCalled()
    expect(sessionStorage.getItem('auth_notice')).toBeNull()
    expect(screen.getByRole('button', {name: 'Update password'})).toBeEnabled()
  })
})
