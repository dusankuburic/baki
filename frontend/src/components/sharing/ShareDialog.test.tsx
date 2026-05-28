import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import ShareDialog from './ShareDialog'
import { useAuthStore } from '@/stores/authStore'

vi.mock('@/api/sharing', () => ({
  sharingApi: {
    listCollaborators: vi.fn().mockResolvedValue([]),
    addCollaborator: vi.fn(),
    updatePermission: vi.fn(),
    removeCollaborator: vi.fn(),
  },
}))

const initialAuthState = useAuthStore.getState()
const defaultProps = {
  flowId: 'flow-1',
  flowName: 'My Flow',
  open: true,
  onClose: vi.fn(),
}

beforeEach(async () => {
  vi.clearAllMocks()
  useAuthStore.setState(initialAuthState, true)
  // Re-import to get the mocked version
  const { sharingApi } = await import('@/api/sharing')
  vi.mocked(sharingApi.listCollaborators).mockResolvedValue([])
})

describe('ShareDialog', () => {
  it('renders nothing when closed', () => {
    render(<ShareDialog {...defaultProps} open={false} />)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('shows the flow name in the title when open', () => {
    render(<ShareDialog {...defaultProps} />)
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(screen.getByText('Share "My Flow"')).toBeInTheDocument()
  })

  it('renders an email input and Invite button', () => {
    render(<ShareDialog {...defaultProps} />)
    expect(screen.getByPlaceholderText('Email address')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /invite/i })).toBeInTheDocument()
  })

  it('Invite button is disabled when email is empty', () => {
    render(<ShareDialog {...defaultProps} />)
    expect(screen.getByRole('button', { name: /invite/i })).toBeDisabled()
  })

  it('Invite button enables when email is entered', () => {
    render(<ShareDialog {...defaultProps} />)
    fireEvent.change(screen.getByPlaceholderText('Email address'), {
      target: { value: 'alice@example.com' },
    })
    expect(screen.getByRole('button', { name: /invite/i })).not.toBeDisabled()
  })

  it('calls addCollaborator when invite form is submitted', async () => {
    const { sharingApi } = await import('@/api/sharing')
    const newCollab = {
      userId: 'user-2', email: 'alice@example.com',
      permission: 'viewer' as const, grantedAt: new Date().toISOString(),
    }
    vi.mocked(sharingApi.addCollaborator).mockResolvedValue(newCollab)

    render(<ShareDialog {...defaultProps} />)
    fireEvent.change(screen.getByPlaceholderText('Email address'), {
      target: { value: 'alice@example.com' },
    })
    fireEvent.click(screen.getByRole('button', { name: /invite/i }))

    await waitFor(() => {
      expect(sharingApi.addCollaborator).toHaveBeenCalledWith({
        flowId: 'flow-1',
        userId: 'alice@example.com',
        permission: 'viewer',
      })
    })
  })

  it('clears the email field after a successful invite', async () => {
    const { sharingApi } = await import('@/api/sharing')
    vi.mocked(sharingApi.addCollaborator).mockResolvedValue({
      userId: 'user-2', email: 'alice@example.com',
      permission: 'viewer', grantedAt: new Date().toISOString(),
    })

    render(<ShareDialog {...defaultProps} />)
    const emailInput = screen.getByPlaceholderText('Email address')
    fireEvent.change(emailInput, { target: { value: 'alice@example.com' } })
    fireEvent.click(screen.getByRole('button', { name: /invite/i }))

    await waitFor(() => {
      expect((emailInput as HTMLInputElement).value).toBe('')
    })
  })

  it('displays error message when addCollaborator fails', async () => {
    const { sharingApi } = await import('@/api/sharing')
    vi.mocked(sharingApi.addCollaborator).mockRejectedValue(new Error('User not found'))

    render(<ShareDialog {...defaultProps} />)
    fireEvent.change(screen.getByPlaceholderText('Email address'), {
      target: { value: 'unknown@example.com' },
    })
    fireEvent.click(screen.getByRole('button', { name: /invite/i }))

    await waitFor(() => {
      expect(screen.getByText('User not found')).toBeInTheDocument()
    })
  })

  it('calls onClose when the close button is clicked', () => {
    const onClose = vi.fn()
    render(<ShareDialog {...defaultProps} onClose={onClose} />)
    fireEvent.click(screen.getByRole('button', { name: /close/i }))
    expect(onClose).toHaveBeenCalledOnce()
  })

  it('shows collaborators list section heading', async () => {
    render(<ShareDialog {...defaultProps} />)
    await waitFor(() => {
      expect(screen.getByText(/people with access/i)).toBeInTheDocument()
    })
  })
})
