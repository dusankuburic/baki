import {describe, it, expect, vi, beforeEach} from 'vitest'
import {useState} from 'react'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ProfileSection} from './ProfileSection'
import {ToastProvider} from '@/components/shared'
import {useAuthStore} from '@/stores/authStore'
import {authApi} from '@/api/auth'

vi.mock('@/api/auth', () => ({
  authApi: {
    updateProfile: vi.fn(),
    logout: vi.fn(),
  },
}))

function Harness({initialEditing = false}: {initialEditing?: boolean}) {
  const [editing, setEditing] = useState(initialEditing)
  return (
    <ToastProvider>
      <ProfileSection editing={editing} onEditingChange={setEditing} />
    </ToastProvider>
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  useAuthStore.setState({user: {id: 'u1', email: 'ada@example.com', role: 'member'}})
})

describe('ProfileSection', () => {
  it('lists the identity fields with honest empty values', () => {
    render(<Harness />)
    expect(screen.getByText('Display name')).toBeInTheDocument()
    expect(screen.getAllByText('Not set')).toHaveLength(2)
    expect(screen.getByText('ada@example.com')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Copy email'})).toBeInTheDocument()
  })

  it('edits and saves the display name and avatar, arming Save only when something changed', async () => {
    vi.mocked(authApi.updateProfile).mockResolvedValue({
      id: 'u1',
      email: 'ada@example.com',
      role: 'member',
      displayName: 'Ada Lovelace',
      avatarUrl: 'https://img.example.com/ada.png',
    })
    render(<Harness />)
    fireEvent.click(screen.getByRole('button', {name: 'Edit'}))
    const save = screen.getByRole('button', {name: 'Save changes'})
    expect(save).toBeDisabled()

    fireEvent.change(screen.getByLabelText('Avatar URL'), {target: {value: 'ftp://img.example.com/ada.png'}})
    expect(screen.getByText('Enter a valid http(s) URL')).toBeInTheDocument()
    expect(save).toBeDisabled()

    fireEvent.change(screen.getByLabelText('Avatar URL'), {target: {value: 'https://img.example.com/ada.png'}})
    fireEvent.change(screen.getByLabelText('Display name'), {target: {value: ' Ada Lovelace '}})
    expect(save).toBeEnabled()
    fireEvent.click(save)

    await waitFor(() =>
      expect(authApi.updateProfile).toHaveBeenCalledWith({
        displayName: 'Ada Lovelace',
        avatarUrl: 'https://img.example.com/ada.png',
      }),
    )
    await waitFor(() => expect(screen.queryByLabelText('Display name')).not.toBeInTheDocument())
    expect(useAuthStore.getState().user?.displayName).toBe('Ada Lovelace')
    expect(screen.getByText('Ada Lovelace')).toBeInTheDocument()
    expect(screen.getByText('https://img.example.com/ada.png')).toBeInTheDocument()
  })

  it('re-seeds the draft each time editing starts and cancels on Escape', () => {
    useAuthStore.setState({user: {id: 'u1', email: 'ada@example.com', role: 'member', displayName: 'Ada'}})
    render(<Harness />)
    fireEvent.click(screen.getByRole('button', {name: 'Edit'}))
    fireEvent.change(screen.getByLabelText('Display name'), {target: {value: 'Scratch'}})
    fireEvent.keyDown(screen.getByLabelText('Display name'), {key: 'Escape'})
    expect(screen.queryByLabelText('Display name')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Edit'}))
    expect(screen.getByLabelText('Display name')).toHaveValue('Ada')
  })
})
