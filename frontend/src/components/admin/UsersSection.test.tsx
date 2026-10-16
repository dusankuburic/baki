import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, within, act} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {useAuthStore} from '@/stores/authStore'
import {useAdminStore} from '@/stores/adminStore'
import {ApiError} from '@/api/client'
import type {AdminUser, UserQuery, UserStats} from '@/api/admin'

const listUsers = vi.fn()
const setUserRole = vi.fn()

vi.mock('@/api/admin', () => ({
  adminApi: {
    listUsers: (...a: unknown[]) => listUsers(...a),
    setUserRole: (...a: unknown[]) => setUserRole(...a),
  },
}))

import {UsersSection} from './UsersSection'

function user(i: number, role: AdminUser['role'] = 'member'): AdminUser {
  return {
    id: `u${i}`,
    email: `user${i}@example.com`,
    role,
    emailVerified: i % 2 === 0,
    displayName: i === 1 ? 'Alice' : '',
    avatarUrl: '',
    createdAt: '2026-09-01T00:00:00Z',
    updatedAt: '2026-09-01T00:00:00Z',
  }
}

const ALL: AdminUser[] = [user(1, 'admin'), ...Array.from({length: 15}, (_, i) => user(i + 2)), user(17, '')]
const STATS: UserStats = {
  total: 17,
  byRole: {admin: 1, member: 15, viewer: 0, guest: 0, none: 1},
  unverified: 8,
  locked: 0,
}

// As the server answers: a cursor (here the offset it stands for) wins over the offset, an offset past 10 000
// without one is refused, and a page with users after it says so and carries the next one's cursor.
function page(query: UserQuery, all: AdminUser[] = ALL) {
  let rows = all
  const q = query.q?.toLowerCase()
  if (q) rows = rows.filter(u => u.email.includes(q) || u.displayName.toLowerCase().includes(q))
  if (query.role) rows = rows.filter(u => (query.role === 'none' ? u.role === '' : u.role === query.role))
  const start = query.cursor ? Number(query.cursor.slice(1)) : query.offset
  if (!query.cursor && start > 10_000) return Promise.reject(new ApiError('at most 10000', 400))
  const more = start + query.limit < rows.length
  return Promise.resolve({
    items: rows.slice(start, start + query.limit),
    total: rows.length,
    offset: query.offset,
    limit: query.limit,
    hasMore: more,
    nextCursor: more ? `c${start + query.limit}` : undefined,
  })
}

function renderSection() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <UsersSection stats={STATS} />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

describe('UsersSection', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    useAdminStore.getState().reset()
    useAuthStore.setState({user: {id: 'u1', email: 'user1@example.com', role: 'admin', displayName: 'Alice'}})
    listUsers.mockImplementation((query: UserQuery) => page(query))
    setUserRole.mockResolvedValue(undefined)
  })

  it('asks for ten users a page and pages from the footer', async () => {
    renderSection()
    expect(await screen.findByText('user2@example.com')).toBeInTheDocument()
    expect(listUsers).toHaveBeenLastCalledWith(expect.objectContaining({limit: 10, offset: 0}), expect.anything())
    const nav = screen.getByRole('navigation', {name: 'Users pagination'})
    expect(nav).toHaveTextContent('Showing 1–10 of 17')
    expect(screen.getAllByRole('row')).toHaveLength(11)

    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    await waitFor(() =>
      expect(listUsers).toHaveBeenLastCalledWith(expect.objectContaining({limit: 10, offset: 10}), expect.anything()),
    )
    expect(await screen.findByText('user17@example.com')).toBeInTheDocument()
    expect(nav).toHaveTextContent('Showing 11–17 of 17')
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
  })

  it("follows the server's cursor to the next page, beside the offset", async () => {
    renderSection()
    await screen.findByText('user2@example.com')
    expect(listUsers).toHaveBeenLastCalledWith(expect.objectContaining({offset: 0}), expect.anything())
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    await waitFor(() =>
      expect(listUsers).toHaveBeenLastCalledWith(
        expect.objectContaining({offset: 10, cursor: 'c10'}),
        expect.anything(),
      ),
    )
    expect(await screen.findByText('user17@example.com')).toBeInTheDocument()
  })

  it('reaches users past offset 10 000 by the cursor', async () => {
    const many = Array.from({length: 20_000}, (_, i) => user(i + 1))
    listUsers.mockImplementation((query: UserQuery) => page(query, many))
    useAdminStore.getState().setUsersPage(1000) // offset 10 000, the deepest the server takes without a cursor
    renderSection()
    expect(await screen.findByText('user10001@example.com')).toBeInTheDocument()
    const nav = screen.getByRole('navigation', {name: 'Users pagination'})
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    expect(await screen.findByText('user10011@example.com')).toBeInTheDocument()
    expect(listUsers).toHaveBeenLastCalledWith(
      expect.objectContaining({offset: 10010, cursor: 'c10010'}),
      expect.anything(),
    )
    expect(nav).toHaveTextContent('Showing 10011–10020 of 20000')
    expect(screen.queryByText("Couldn't load users")).toBeNull()
  })

  it('waits for the page it asked for before Next leads on', async () => {
    const many = Array.from({length: 20_000}, (_, i) => user(i + 1))
    listUsers.mockImplementation((query: UserQuery) => page(query, many))
    useAdminStore.getState().setUsersPage(1000)
    renderSection()
    await screen.findByText('user10001@example.com')
    let release: () => void = () => {}
    listUsers.mockImplementationOnce(
      (query: UserQuery) => new Promise(resolve => (release = () => resolve(page(query, many)))),
    )
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    // A second click now would ask for page 1002 before page 1001 told its cursor: past 10 000, a refused offset.
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    act(() => release())
    expect(await screen.findByText('user10011@example.com')).toBeInTheDocument()
    expect(useAdminStore.getState().users.page).toBe(1001)
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    expect(await screen.findByText('user10021@example.com')).toBeInTheDocument()
    expect(listUsers).toHaveBeenLastCalledWith(
      expect.objectContaining({offset: 10020, cursor: 'c10020'}),
      expect.anything(),
    )
  })

  it('keeps Next off after a page fails, until that page answers', async () => {
    const many = Array.from({length: 20_000}, (_, i) => user(i + 1))
    listUsers.mockImplementation((query: UserQuery) => page(query, many))
    useAdminStore.getState().setUsersPage(1000)
    renderSection()
    await screen.findByText('user10001@example.com')
    listUsers.mockImplementationOnce(() => Promise.reject(new ApiError('network', 0)))
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    await waitFor(() => expect(listUsers).toHaveBeenCalledTimes(2))
    expect(await screen.findByText("Couldn't load users")).toBeInTheDocument()
    // The failed page told no cursor: the page after it has none, and past 10 000 its offset is refused.
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
    expect(screen.getByRole('button', {name: 'Previous page'})).toBeEnabled()
    act(() => useAdminStore.getState().refresh())
    expect(await screen.findByText('user10011@example.com')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByRole('button', {name: 'Next page'})).toBeEnabled())
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    expect(await screen.findByText('user10021@example.com')).toBeInTheDocument()
    expect(listUsers).toHaveBeenLastCalledWith(
      expect.objectContaining({offset: 10020, cursor: 'c10020'}),
      expect.anything(),
    )
  })

  it('takes the server at its word that no page follows, whatever the count says', async () => {
    // The count was made before users were removed: 30, of which 17 remain.
    listUsers.mockImplementation((query: UserQuery) => page(query).then(res => ({...res, total: 30})))
    renderSection()
    await screen.findByText('user2@example.com')
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    expect(await screen.findByText('user17@example.com')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
  })

  it('changes the page size from the footer', async () => {
    renderSection()
    await screen.findByText('user2@example.com')
    fireEvent.change(screen.getByRole('combobox', {name: 'Rows per page'}), {target: {value: '25'}})
    await waitFor(() =>
      expect(listUsers).toHaveBeenLastCalledWith(expect.objectContaining({limit: 25, offset: 0}), expect.anything()),
    )
    expect(await screen.findByText('user17@example.com')).toBeInTheDocument()
  })

  it('searches after the debounce and restarts from the first page', async () => {
    renderSection()
    await screen.findByText('user2@example.com')
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    await waitFor(() =>
      expect(listUsers).toHaveBeenLastCalledWith(expect.objectContaining({offset: 10}), expect.anything()),
    )

    fireEvent.change(screen.getByRole('textbox', {name: 'Search by email or name'}), {target: {value: 'ali'}})
    await waitFor(() =>
      expect(listUsers).toHaveBeenLastCalledWith(expect.objectContaining({q: 'ali', offset: 0}), expect.anything()),
    )
    expect(await screen.findByText('Alice')).toBeInTheDocument()
    expect(screen.getByRole('navigation', {name: 'Users pagination'})).toHaveTextContent('Showing 1–1 of 1')
  })

  it('filters by role from the chips, whose counts cover the whole table', async () => {
    renderSection()
    await screen.findByText('user2@example.com')
    const members = screen.getByRole('button', {name: 'Member, 15 users'})
    fireEvent.click(members)
    await waitFor(() =>
      expect(listUsers).toHaveBeenLastCalledWith(
        expect.objectContaining({role: 'member', offset: 0}),
        expect.anything(),
      ),
    )
    expect(members).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', {name: 'Viewer, 0 users'})).toBeDisabled()
    expect(screen.getByRole('button', {name: 'Admin, 1 user'})).toBeEnabled()
  })

  it('shows an account without a valid role as unassigned, never as Admin', async () => {
    useAdminStore.getState().setUsersRole('none')
    renderSection()
    expect(await screen.findByText('user17@example.com')).toBeInTheDocument()
    expect(screen.getByRole('cell', {name: 'Unassigned'})).toBeInTheDocument()
    const select = screen.getByRole('combobox', {name: 'Change role for user17@example.com'})
    expect(select).toHaveValue('')
    expect(screen.getByRole('option', {name: 'Choose a role'})).toBeDisabled()
  })

  it('changes a role after one confirmation and refetches the page', async () => {
    renderSection()
    await screen.findByText('user2@example.com')
    fireEvent.change(screen.getByRole('combobox', {name: 'Change role for user2@example.com'}), {
      target: {value: 'viewer'},
    })
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent('Change user2@example.com from Member to Viewer?')
    fireEvent.click(within(dialog).getByRole('button', {name: 'Change role'}))
    await waitFor(() => expect(setUserRole).toHaveBeenCalledWith('u2', 'viewer'))
    await waitFor(() => expect(listUsers).toHaveBeenCalledTimes(2))
    expect(await screen.findByText('Role updated')).toBeInTheDocument()
  })

  it('warns before removing your own admin access, and does nothing when cancelled', async () => {
    renderSection()
    await screen.findByText('user2@example.com')
    fireEvent.change(screen.getByRole('combobox', {name: 'Change role for user1@example.com'}), {
      target: {value: 'member'},
    })
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent('Remove your admin access')
    fireEvent.click(within(dialog).getByRole('button', {name: 'Cancel'}))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(setUserRole).not.toHaveBeenCalled()
    expect(screen.getByRole('combobox', {name: 'Change role for user1@example.com'})).toHaveValue('admin')
  })

  it('surfaces the server refusal when the last admin would be demoted', async () => {
    setUserRole.mockRejectedValue(new Error('cannot demote the last administrator'))
    renderSection()
    await screen.findByText('user2@example.com')
    fireEvent.change(screen.getByRole('combobox', {name: 'Change role for user1@example.com'}), {
      target: {value: 'member'},
    })
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', {name: 'Remove access'}))
    expect(await screen.findByText('cannot demote the last administrator')).toBeInTheDocument()
  })

  it('offers to clear the filters when nothing matches', async () => {
    renderSection()
    await screen.findByText('user2@example.com')
    fireEvent.change(screen.getByRole('textbox', {name: 'Search by email or name'}), {target: {value: 'zzz'}})
    expect(await screen.findByText('No users match')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Clear filters'}))
    await waitFor(() =>
      expect(listUsers).toHaveBeenLastCalledWith(expect.objectContaining({offset: 0}), expect.anything()),
    )
    expect(listUsers.mock.calls.at(-1)?.[0]).not.toHaveProperty('q', 'zzz')
    expect(await screen.findByText('user2@example.com')).toBeInTheDocument()
  })
})
