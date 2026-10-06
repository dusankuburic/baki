import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {useAdminStore} from '@/stores/adminStore'
import type {AuditEvent, AuditFilter} from '@/api/admin'

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

function respond(filter: AuditFilter) {
  let rows = EVENTS
  if (filter.action) rows = rows.filter(e => e.action === filter.action)
  if (filter.userId) rows = rows.filter(e => e.userId === filter.userId)
  const offset = filter.offset ?? 0
  return Promise.resolve(rows.slice(offset, offset + (filter.limit ?? 100)))
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
    expect(screen.getAllByRole('row')).toHaveLength(16) // the header row and fifteen events
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

  it('disables Next on an exactly full last page', async () => {
    listAuditEvents.mockResolvedValue(EVENTS.slice(0, 15))
    renderSection()
    await screen.findAllByText('Changed settings')
    expect(screen.getAllByRole('row')).toHaveLength(16) // the header row and fifteen events
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
    listAuditEvents.mockResolvedValue([event(1, {meta: {new_role: 'member'}}), event(2, {meta: null})])
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
      .mockResolvedValueOnce([
        event(1, {userId: '', email: '', action: 'auth.login_failure', resourceType: 'user', resourceId: 'u9'}),
      ])
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
    listAuditEvents.mockResolvedValue([])
    renderSection()
    expect(await screen.findByText('No audit events yet')).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Clear filters'})).not.toBeInTheDocument()
  })
})
