import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, act} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {useAdminStore} from '@/stores/adminStore'
import {ApiError} from '@/api/client'
import type {AuditEvent, AuditFilter, AuditPage} from '@/api/admin'

const listAuditEvents = vi.fn()
const listAuditActions = vi.fn()

vi.mock('@/api/admin', () => ({
  adminApi: {
    listAuditEvents: (...a: unknown[]) => listAuditEvents(...a),
    listAuditActions: (...a: unknown[]) => listAuditActions(...a),
  },
}))

import {AuditLogSection} from './AuditLogSection'

function event(i: number, over: Partial<AuditEvent> = {}): AuditEvent {
  return {
    id: `e${i}`,
    userId: 'u1',
    email: 'admin@example.com',
    action: 'settings.change',
    resourceType: 'settings',
    resourceId: 'ab1f6fc3-5359-49da',
    ip: '172.21.0.1',
    meta: {scope: 'user'},
    createdAt: `2026-09-25T21:${String(i).padStart(2, '0')}:00Z`,
    ...over,
  }
}

const EVENTS = Array.from({length: 40}, (_, i) => event(i))

function page(events: AuditEvent[], nextCursor?: string): AuditPage {
  return {events, nextCursor}
}

// As the server answers: a cursor (here the offset it stands for) wins over the offset, an offset past 10 000
// without one is refused, and X-Next-Cursor comes while events follow.
function respond(filter: AuditFilter, rows: (from: number, to: number) => AuditEvent[] = filtered(filter)) {
  const offset = filter.cursor ? Number(filter.cursor.slice(1)) : (filter.offset ?? 0)
  if (!filter.cursor && offset > 10_000) return Promise.reject(new ApiError('at most 10000', 400))
  const limit = filter.limit ?? 100
  const out = rows(offset, offset + limit + 1)
  return Promise.resolve(page(out.slice(0, limit), out.length > limit ? `c${offset + limit}` : undefined))
}

function filtered(filter: AuditFilter) {
  let rows = EVENTS
  if (filter.action) rows = rows.filter(e => e.action === filter.action)
  if (filter.userId) rows = rows.filter(e => e.userId === filter.userId)
  return (from: number, to: number) => rows.slice(from, to)
}

// A log of n events, made as they are asked for.
function longLog(n: number) {
  return (from: number, to: number) =>
    Array.from({length: Math.max(0, Math.min(to, n) - from)}, (_, i) => event(from + i))
}

function renderSection() {
  return render(
    <ToastProvider>
      <AuditLogSection />
    </ToastProvider>,
  )
}

describe('AuditLogSection', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    useAdminStore.getState().reset()
    listAuditEvents.mockImplementation((filter: AuditFilter) => respond(filter))
    listAuditActions.mockResolvedValue(['auth.login', 'auth.login_failure', 'settings.change', 'org_channel_save'])
  })

  it('asks for one row beyond the page of 15 and pages from the footer', async () => {
    renderSection()
    await screen.findAllByText('Changed settings')
    expect(listAuditEvents).toHaveBeenLastCalledWith(expect.objectContaining({limit: 16, offset: 0}), expect.anything())
    expect(screen.getAllByRole('row')).toHaveLength(16)
    const nav = screen.getByRole('navigation', {name: 'Audit log pagination'})
    expect(nav).toHaveTextContent('Showing 1–15')
    expect(screen.getByRole('button', {name: 'Next page'})).toBeEnabled()

    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    await waitFor(() =>
      expect(listAuditEvents).toHaveBeenLastCalledWith(
        expect.objectContaining({limit: 16, offset: 15}),
        expect.anything(),
      ),
    )
    await waitFor(() => expect(nav).toHaveTextContent('Showing 16–30'))

    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    await waitFor(() => expect(nav).toHaveTextContent('Showing 31–40'))
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
  })

  it('follows X-Next-Cursor past the offset cap of 10 000', async () => {
    listAuditEvents.mockImplementation((filter: AuditFilter) => respond(filter, longLog(20_000)))
    useAdminStore.getState().setAuditPage(666) // offset 9990: the next page would start at 10 005
    renderSection()
    await screen.findAllByText('Changed settings')
    const next = screen.getByRole('button', {name: 'Next page'})
    expect(next).toBeEnabled()
    // The last page in offset reach asks for its own events: the cursor it answers leads past the cap.
    expect(listAuditEvents).toHaveBeenLastCalledWith(
      expect.objectContaining({offset: 9990, limit: 15, cursor: undefined}),
      expect.anything(),
    )

    fireEvent.click(next)
    const nav = screen.getByRole('navigation', {name: 'Audit log pagination'})
    await waitFor(() => expect(nav).toHaveTextContent('Showing 10006–10020'))
    expect(listAuditEvents).toHaveBeenLastCalledWith(
      expect.objectContaining({cursor: 'c10005', limit: 15}),
      expect.anything(),
    )
    expect(screen.queryByText("Couldn't load the audit log")).toBeNull()
    await waitFor(() => expect(screen.getByRole('button', {name: 'Next page'})).toBeEnabled())
  })

  it('against a server without cursors, keeps a deep full page open until the page after it is empty', async () => {
    // The release before: no cursor, no offset cap; 10 005 events end a full page.
    listAuditEvents.mockImplementation((filter: AuditFilter) => {
      const from = filter.offset ?? 0
      return Promise.resolve(page(longLog(10_005)(from, from + (filter.limit ?? 100))))
    })
    useAdminStore.getState().setAuditPage(666)
    renderSection()
    await screen.findAllByText('Changed settings')
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    await waitFor(() =>
      expect(listAuditEvents).toHaveBeenLastCalledWith(expect.objectContaining({offset: 10005}), expect.anything()),
    )
    await waitFor(() => expect(useAdminStore.getState().audit.page).toBe(666))
    await waitFor(() => expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled())
    expect(screen.getByRole('navigation', {name: 'Audit log pagination'})).toHaveTextContent('Showing 9991–10005')
  })

  it('waits for the page it asked for before Next leads on', async () => {
    let release: () => void = () => {}
    renderSection()
    await screen.findAllByText('Changed settings')
    listAuditEvents.mockImplementationOnce(
      (filter: AuditFilter) => new Promise(resolve => (release = () => resolve(respond(filter)))),
    )
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
    release()
    await waitFor(() => expect(screen.getByRole('button', {name: 'Next page'})).toBeEnabled())
  })

  it('keeps Next off after a page fails, until that page answers', async () => {
    listAuditEvents.mockImplementation((filter: AuditFilter) => respond(filter, longLog(50_000)))
    useAdminStore.getState().setAuditPage(666)
    renderSection()
    await screen.findAllByText('Changed settings')
    listAuditEvents.mockImplementationOnce(() => Promise.reject(new ApiError('network', 0)))
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    expect(await screen.findByText("Couldn't load the audit log")).toBeInTheDocument()
    // The failed page told no cursor: the page after it has none, and past 10 000 its offset is refused.
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
    act(() => useAdminStore.getState().refresh())
    const nav = screen.getByRole('navigation', {name: 'Audit log pagination'})
    await waitFor(() => expect(nav).toHaveTextContent('Showing 10006–10020'))
    await waitFor(() => expect(screen.getByRole('button', {name: 'Next page'})).toBeEnabled())
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    await waitFor(() => expect(nav).toHaveTextContent('Showing 10021–10035'))
    expect(listAuditEvents).toHaveBeenLastCalledWith(
      expect.objectContaining({cursor: 'c10020', limit: 15}),
      expect.anything(),
    )
  })

  it('disables Next on an exactly full last page', async () => {
    listAuditEvents.mockResolvedValue(page(EVENTS.slice(0, 15)))
    renderSection()
    await screen.findAllByText('Changed settings')
    expect(screen.getAllByRole('row')).toHaveLength(16)
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
  })

  it('groups the actions with human labels and filters by one, from the first page', async () => {
    renderSection()
    await screen.findAllByText('Changed settings')
    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    await waitFor(() =>
      expect(listAuditEvents).toHaveBeenLastCalledWith(expect.objectContaining({offset: 15}), expect.anything()),
    )

    expect(await screen.findByRole('group', {name: 'Organisations'})).toBeInTheDocument()
    expect(screen.getByRole('option', {name: 'Saved a notification channel'})).toHaveValue('org_channel_save')
    fireEvent.change(screen.getByRole('combobox', {name: 'Filter by action'}), {target: {value: 'auth.login'}})
    await waitFor(() =>
      expect(listAuditEvents).toHaveBeenLastCalledWith(
        expect.objectContaining({action: 'auth.login', offset: 0}),
        expect.anything(),
      ),
    )
    expect(await screen.findByText('No events match')).toBeInTheDocument()
    fireEvent.click(screen.getAllByRole('button', {name: 'Clear filters'})[0]!)
    await waitFor(() => expect(useAdminStore.getState().audit.action).toBe(''))
    await waitFor(() => expect(screen.getAllByRole('row')).toHaveLength(16))
  })

  it("reveals an event's metadata on request, and says when there is none", async () => {
    listAuditEvents.mockResolvedValue(page([event(1, {meta: {new_role: 'member'}}), event(2, {meta: null})]))
    renderSection()
    await screen.findAllByText('Changed settings')
    const toggles = screen.getAllByRole('button', {name: 'Show details'})
    expect(toggles).toHaveLength(2)
    fireEvent.click(toggles[0]!)
    expect(screen.getByText('new_role')).toBeInTheDocument()
    expect(screen.getByText('member')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Hide details'})).toHaveAttribute('aria-expanded', 'true')
    fireEvent.click(toggles[1]!)
    expect(screen.getByText('No additional details')).toBeInTheDocument()
  })

  it('filters by the user of a row, including the subject of a failed sign-in', async () => {
    listAuditEvents
      .mockResolvedValueOnce(
        page([event(1, {userId: '', email: '', action: 'auth.login_failure', resourceType: 'user', resourceId: 'u9'})]),
      )
      .mockImplementation((filter: AuditFilter) => respond(filter))
    renderSection()
    const subject = await screen.findByRole('button', {name: 'user u9'})
    expect(subject).toHaveAttribute('title', 'Show events for u9')
    expect(screen.getByRole('cell', {name: 'Failed sign-in'})).toBeInTheDocument()
    fireEvent.click(subject)
    await waitFor(() =>
      expect(listAuditEvents).toHaveBeenLastCalledWith(
        expect.objectContaining({userId: 'u9', offset: 0}),
        expect.anything(),
      ),
    )
    expect(screen.getByRole('textbox', {name: 'Filter by user ID'})).toHaveValue('u9')
  })

  it('tells an empty log from an empty filter result', async () => {
    listAuditEvents.mockResolvedValue(page([]))
    renderSection()
    expect(await screen.findByText('No audit events yet')).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Clear filters'})).not.toBeInTheDocument()
  })
})
