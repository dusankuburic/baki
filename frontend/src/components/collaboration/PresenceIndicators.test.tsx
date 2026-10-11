import {describe, it, expect, beforeEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import PresenceIndicators from './PresenceIndicators'
import {usePresenceStore} from '@/stores/presenceStore'
import type {ConnectionStatus} from '@/services/collaboration/CollaborationService'
import type {PresenceUser} from '@/stores/presenceStore'

const initialState = usePresenceStore.getState()

function setPresence(users: Record<string, PresenceUser>, status: ConnectionStatus = 'connected') {
  usePresenceStore.setState({...initialState, users, status}, true)
}

beforeEach(() => {
  usePresenceStore.setState(initialState, true)
})

describe('PresenceIndicators', () => {
  it('renders nothing when disconnected', () => {
    usePresenceStore.setState({...initialState, status: 'disconnected', users: {}}, true)
    const {container} = render(<PresenceIndicators />)
    expect(container.firstChild).toBeNull()
  })

  it('renders nothing when no users are present', () => {
    setPresence({}, 'connected')
    const {container} = render(<PresenceIndicators />)
    expect(container.firstChild).toBeNull()
  })

  it('renders an avatar for each visible user', () => {
    setPresence({
      u1: {userId: 'u1', displayName: 'Alice'},
      u2: {userId: 'u2', displayName: 'Bob'},
    })
    render(<PresenceIndicators maxVisible={5} />)
    expect(screen.getByText('AL')).toBeInTheDocument()
    expect(screen.getByText('BO')).toBeInTheDocument()
  })

  it('shows overflow badge when users exceed maxVisible', () => {
    const users: Record<string, PresenceUser> = {}
    for (let i = 0; i < 6; i++) {
      users[`u${i}`] = {userId: `u${i}`, displayName: `User${i}`}
    }
    setPresence(users)
    render(<PresenceIndicators maxVisible={3} />)
    expect(screen.getByText('+3')).toBeInTheDocument()
  })

  it('shows green dot when connected', () => {
    setPresence({u1: {userId: 'u1', displayName: 'Alice'}}, 'connected')
    const {container} = render(<PresenceIndicators />)
    const dot = container.querySelector('.bg-semantic-success')
    expect(dot).toBeTruthy()
  })

  it('shows yellow dot when connecting', () => {
    setPresence({u1: {userId: 'u1', displayName: 'Alice'}}, 'connecting')
    const {container} = render(<PresenceIndicators />)
    const dot = container.querySelector('.bg-semantic-warning')
    expect(dot).toBeTruthy()
  })
})

describe('PresenceIndicators socket errors', () => {
  it('toasts a server error envelope instead of dropping it', async () => {
    const {ToastProvider} = await import('@/components/shared')
    setPresence({u1: {userId: 'u1', displayName: 'Ann'}}, 'connected')
    render(
      <ToastProvider>
        <PresenceIndicators />
      </ToastProvider>,
    )
    usePresenceStore.setState({
      lastError: {code: 'permission_denied', message: 'editor access is required to modify this flow', at: 1},
    })
    expect(await screen.findByText('editor access is required to modify this flow')).toBeInTheDocument()
    expect(screen.getByText('permission_denied')).toBeInTheDocument()
  })

  it('renders without a ToastProvider (isolated mounts skip the toast)', () => {
    setPresence({u1: {userId: 'u1', displayName: 'Ann'}}, 'connected')
    usePresenceStore.setState({lastError: {code: 'unknown_event', message: 'nope', at: 2}})
    expect(() => render(<PresenceIndicators />)).not.toThrow()
  })
})
