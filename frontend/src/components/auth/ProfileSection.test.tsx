import {describe, it, expect, vi, beforeEach} from 'vitest'
import {useState} from 'react'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ProfileSection} from './ProfileSection'
import {ToastProvider} from '@/components/shared'
import {useAuthStore} from '@/stores/authStore'
import {authApi} from '@/api/auth'
import {downscaleImage} from '@/lib/imageResize'

vi.mock('@/api/auth', () => ({
  authApi: {
    updateProfile: vi.fn(),
    logout: vi.fn(),
    uploadAvatar: vi.fn(),
    deleteAvatar: vi.fn(),
  },
}))

vi.mock('@/lib/imageResize', () => ({
  downscaleImage: vi.fn(async (file: Blob) => file),
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

  it('uploads a picture: downscaled, sent, and the profile follows the serving route', async () => {
    useAuthStore.setState({user: {id: 'u1', email: 'ada@example.com', role: 'member', displayName: 'Ada'}})
    vi.mocked(authApi.uploadAvatar).mockResolvedValue({
      id: 'u1',
      email: 'ada@example.com',
      role: 'member',
      displayName: 'Ada',
      avatarUrl: '/api/users/u1/avatar',
    })
    const {container} = render(<Harness initialEditing />)
    const file = new File(['png-bytes'], 'me.png', {type: 'image/png'})
    fireEvent.change(container.querySelector('input[type=file]')!, {target: {files: [file]}})
    await waitFor(() => expect(authApi.uploadAvatar).toHaveBeenCalled())
    expect(downscaleImage).toHaveBeenCalledWith(file)
    await waitFor(() => expect(useAuthStore.getState().user?.avatarUrl).toBe('/api/users/u1/avatar'))
    // The URL field follows the upload, so saving the form keeps it.
    expect(screen.getByLabelText('Avatar URL')).toHaveValue('/api/users/u1/avatar')
    expect(screen.getByRole('button', {name: 'Remove picture'})).toBeInTheDocument()
  })

  it('refuses a non-PNG/JPEG file before anything is sent', async () => {
    useAuthStore.setState({user: {id: 'u1', email: 'ada@example.com', role: 'member'}})
    const {container} = render(<Harness initialEditing />)
    fireEvent.change(container.querySelector('input[type=file]')!, {
      target: {files: [new File(['gif!'], 'me.gif', {type: 'image/gif'})]},
    })
    expect(await screen.findByRole('alert')).toHaveTextContent('Pictures must be PNG or JPEG images')
    expect(authApi.uploadAvatar).not.toHaveBeenCalled()
  })

  it('removes the uploaded picture and clears the profile field', async () => {
    useAuthStore.setState({
      user: {id: 'u1', email: 'ada@example.com', role: 'member', avatarUrl: '/api/users/u1/avatar'},
    })
    vi.mocked(authApi.deleteAvatar).mockResolvedValue({
      id: 'u1',
      email: 'ada@example.com',
      role: 'member',
      avatarUrl: '',
    })
    render(<Harness initialEditing />)
    fireEvent.click(screen.getByRole('button', {name: 'Remove picture'}))
    await waitFor(() => expect(useAuthStore.getState().user?.avatarUrl).toBe(''))
    expect(screen.getByLabelText('Avatar URL')).toHaveValue('')
    expect(screen.queryByRole('button', {name: 'Remove picture'})).not.toBeInTheDocument()
  })

  it('saves a new display name while the uploaded picture stays: its route is a valid value', async () => {
    useAuthStore.setState({
      user: {id: 'u1', email: 'ada@example.com', role: 'member', avatarUrl: '/api/users/u1/avatar'},
    })
    vi.mocked(authApi.updateProfile).mockResolvedValue({
      id: 'u1',
      email: 'ada@example.com',
      role: 'member',
      displayName: 'Ada',
      avatarUrl: '/api/users/u1/avatar',
    })
    render(<Harness initialEditing />)
    fireEvent.change(screen.getByLabelText('Display name'), {target: {value: 'Ada'}})
    expect(screen.queryByText('Enter a valid http(s) URL')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Save changes'}))
    await waitFor(() =>
      expect(authApi.updateProfile).toHaveBeenCalledWith({displayName: 'Ada', avatarUrl: '/api/users/u1/avatar'}),
    )
  })

  it('still refuses a hand-typed relative path that is not the upload', () => {
    useAuthStore.setState({
      user: {id: 'u1', email: 'ada@example.com', role: 'member', avatarUrl: '/api/users/u1/avatar'},
    })
    render(<Harness initialEditing />)
    fireEvent.change(screen.getByLabelText('Avatar URL'), {target: {value: '/api/users/u2/avatar'}})
    expect(screen.getByText('Enter a valid http(s) URL')).toBeInTheDocument()
  })
})
