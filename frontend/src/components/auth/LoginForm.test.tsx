import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import LoginForm from './LoginForm'
import { useAuthStore } from '@/stores/authStore'

// Snapshot the initial store state for full reset between tests
const initialState = useAuthStore.getState()

beforeEach(() => {
  useAuthStore.setState(initialState, true)
  vi.resetAllMocks()
})

describe('LoginForm', () => {
  it('renders email and password fields', () => {
    render(<LoginForm />)
    expect(screen.getByPlaceholderText('Email')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('Password')).toBeInTheDocument()
  })

  it('submit button is disabled when fields are empty', () => {
    render(<LoginForm />)
    expect(screen.getByRole('button', { name: /sign in/i })).toBeDisabled()
  })

  it('submit button enables when both fields are filled', () => {
    render(<LoginForm />)
    fireEvent.change(screen.getByPlaceholderText('Email'), { target: { value: 'a@b.com' } })
    fireEvent.change(screen.getByPlaceholderText('Password'), { target: { value: 'secret' } })
    expect(screen.getByRole('button', { name: /sign in/i })).not.toBeDisabled()
  })

  it('calls login with the entered credentials', async () => {
    const loginMock = vi.fn().mockResolvedValue(undefined)
    useAuthStore.setState({ ...initialState, login: loginMock }, true)

    render(<LoginForm />)
    fireEvent.change(screen.getByPlaceholderText('Email'), { target: { value: 'user@example.com' } })
    fireEvent.change(screen.getByPlaceholderText('Password'), { target: { value: 'pass123' } })
    fireEvent.click(screen.getByRole('button', { name: /sign in/i }))

    await waitFor(() => {
      expect(loginMock).toHaveBeenCalledWith({
        email: 'user@example.com',
        password: 'pass123',
      })
    })
  })

  it('calls onSuccess after successful login', async () => {
    const loginMock = vi.fn().mockResolvedValue(undefined)
    const onSuccess = vi.fn()
    useAuthStore.setState({ ...initialState, login: loginMock }, true)

    render(<LoginForm onSuccess={onSuccess} />)
    fireEvent.change(screen.getByPlaceholderText('Email'), { target: { value: 'u@e.com' } })
    fireEvent.change(screen.getByPlaceholderText('Password'), { target: { value: 'pw' } })
    fireEvent.click(screen.getByRole('button', { name: /sign in/i }))

    await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce())
  })

  it('displays error message from store', () => {
    useAuthStore.setState({ ...initialState, error: 'Invalid credentials' }, true)
    render(<LoginForm />)
    expect(screen.getByText('Invalid credentials')).toBeInTheDocument()
  })

  it('does not display error when error is null', () => {
    useAuthStore.setState({ ...initialState, error: null }, true)
    render(<LoginForm />)
    expect(screen.queryByText('Invalid credentials')).not.toBeInTheDocument()
  })
})
