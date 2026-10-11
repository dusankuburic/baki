import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, within} from '@testing-library/react'
import {UserProfile} from './UserProfile'
import {ToastProvider, ConfirmProvider} from '@/components/shared'
import {useAuthStore} from '@/stores/authStore'
import {useOrgStore} from '@/stores/orgStore'

vi.mock('@/api/auth', () => ({
  authApi: {
    listSessions: vi.fn().mockResolvedValue([]),
    revokeSession: vi.fn(),
    ssoInfo: vi.fn().mockResolvedValue({enabled: false}),
    ssoLinks: vi.fn().mockResolvedValue([]),
    updateProfile: vi.fn(),
    changePassword: vi.fn(),
    exportAccount: vi.fn(),
    deleteAccount: vi.fn(),
    logout: vi.fn().mockResolvedValue(undefined),
  },
}))

const logout = vi.fn().mockResolvedValue(undefined)
const loadOrgs = vi.fn().mockResolvedValue(undefined)

function renderProfile() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <UserProfile />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  useAuthStore.setState({
    user: {id: 'u1', email: 'ada@example.com', role: 'admin', displayName: 'Ada', createdAt: '2026-05-04T10:00:00Z'},
    isAuthenticated: true,
    logout,
  })
  useOrgStore.setState({
    organisations: [
      {
        id: 'org-1',
        name: 'RPA-org',
        ownerId: 'u1',
        members: [{userId: 'u1', role: 'admin', joinedAt: ''}],
        createdAt: '',
        updatedAt: '',
      },
    ],
    isLoading: false,
    loadOrgs,
  })
})

describe('UserProfile', () => {
  it('renders the hero, the section nav and the three groups in order', async () => {
    renderProfile()
    const hero = document.querySelector<HTMLElement>('.profile-hero')!
    expect(within(hero).getByRole('heading', {level: 1, name: 'Ada'})).toBeInTheDocument()
    expect(within(hero).getByText('ada@example.com')).toBeInTheDocument()
    expect(within(hero).getByText('admin')).toBeInTheDocument()
    expect(within(hero).getByText('1 organisation')).toBeInTheDocument()
    expect(within(hero).getByText(/Member since/)).toBeInTheDocument()

    const nav = screen.getByRole('navigation', {name: 'Profile sections'})
    const items = within(nav).getAllByRole('button')
    expect(items.map(b => b.textContent)).toEqual(['Account', 'Security', 'Data & privacy'])
    expect(items[0]).toHaveAttribute('aria-current', 'true')

    const groups = screen.getAllByRole('heading', {level: 2}).map(h => h.textContent)
    expect(groups).toEqual(['Account', 'Security', 'Data & privacy'])
    const cards = screen.getAllByRole('heading', {level: 3}).map(h => h.textContent)
    expect(cards[0]).toBe('Profile')
    expect(cards[cards.length - 1]).toBe('Danger zone')
    expect(await screen.findByText('RPA-org')).toBeInTheDocument()
    expect(loadOrgs).toHaveBeenCalled()
  })

  it('"Edit profile" in the hero opens the identity form; "Sign out" signs out', () => {
    renderProfile()
    expect(screen.queryByLabelText('Display name')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Edit profile'}))
    expect(screen.getByLabelText('Display name')).toHaveValue('Ada')
    fireEvent.click(screen.getByRole('button', {name: 'Cancel'}))
    expect(screen.queryByLabelText('Display name')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'Sign out'}))
    expect(logout).toHaveBeenCalledTimes(1)
  })

  it('the nav marks the section it jumped to as current and moves focus to its heading', () => {
    renderProfile()
    const nav = screen.getByRole('navigation', {name: 'Profile sections'})
    fireEvent.click(within(nav).getByRole('button', {name: 'Security'}))
    expect(within(nav).getByRole('button', {name: 'Security'})).toHaveAttribute('aria-current', 'true')
    expect(within(nav).getByRole('button', {name: 'Account'})).not.toHaveAttribute('aria-current')
    expect(screen.getByRole('heading', {level: 2, name: 'Security'})).toHaveFocus()
  })

  it('renders nothing for a signed-out user in cloud mode', () => {
    useAuthStore.setState({user: null, isAuthenticated: false})
    const {container} = renderProfile()
    expect(container.querySelector('.profile-page')).toBeNull()
  })
})
