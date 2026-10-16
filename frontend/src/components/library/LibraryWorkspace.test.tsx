import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, act, within} from '@testing-library/react'
import LibraryWorkspace from './LibraryWorkspace'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {useLibraryBrowseStore} from '@/stores/libraryBrowseStore'
import {useOrgStore} from '@/stores/orgStore'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {VersionConflictError} from '@/api/client'

const list = vi.fn()
const get = vi.fn()
const getContent = vi.fn()
const versions = vi.fn()
const update = vi.fn()
const create = vi.fn()
const setTags = vi.fn()

vi.mock('@/api/flow', () => ({
  flowApi: {
    setTags: (...a: unknown[]) => setTags(...a),
  },
}))

vi.mock('@/api/library', async () => {
  const actual = await vi.importActual<typeof import('@/api/library')>('@/api/library')
  return {
    ...actual,
    libraryApi: {
      list: (...a: unknown[]) => list(...a),
      get: (...a: unknown[]) => get(...a),
      getContent: (...a: unknown[]) => getContent(...a),
      versions: (...a: unknown[]) => versions(...a),
      delete: vi.fn(),
      create: (...a: unknown[]) => create(...a),
      update: (...a: unknown[]) => update(...a),
    },
  }
})

function flow(id: string, overrides: Partial<{name: string; orgId: string; isShared: boolean}> = {}) {
  return {
    id,
    name: overrides.name ?? `Flow ${id}`,
    ownerId: 'me',
    ownerDisplayName: 'me@example.com',
    organizationId: overrides.orgId ?? '',
    blockCount: 10,
    subflowCount: 2,
    updatedAt: '2026-01-01T00:00:00Z',
    version: 1,
    isSharedWithMe: overrides.isShared ?? false,
    canEdit: true,
    canDelete: true,
    canShare: true,
  }
}

function renderWorkspace() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <LibraryWorkspace />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  useLibraryBrowseStore.getState().reset()
  useOrgStore.setState({organisations: [], activeOrgId: null, isLoading: false, isBusy: false, error: null})
  useFlowStore.setState({document: null, libraryFlowId: null, libraryVersion: 0})
  useUIStore.setState({mainPaneView: 'library'})
  list.mockResolvedValue({items: [], total: 0, offset: 0, limit: 24})
})

describe('LibraryWorkspace', () => {
  it('shows skeleton while loading and the empty state when the list is empty', async () => {
    renderWorkspace()
    expect(document.querySelector('.animate-pulse')).toBeTruthy()
    expect(await screen.findByText(/No flows yet/)).toBeInTheDocument()
  })

  it('renders the page of flows from the list endpoint', async () => {
    list.mockResolvedValue({items: [flow('a'), flow('b', {name: 'Beta'})], total: 2, offset: 0, limit: 24})
    renderWorkspace()
    expect(await screen.findByText('Flow a')).toBeInTheDocument()
    expect(screen.getByText('Beta')).toBeInTheDocument()
    expect(screen.getByText(/2 flows/)).toBeInTheDocument()
  })

  it('refetches with scope=mine when the user clicks "My flows"', async () => {
    list.mockResolvedValue({items: [flow('a')], total: 1, offset: 0, limit: 24})
    renderWorkspace()
    await screen.findByText('Flow a')
    list.mockClear()
    fireEvent.click(screen.getByText('My flows'))
    await waitFor(() => {
      expect(list).toHaveBeenCalled()
    })
    const callArgs = list.mock.calls.at(-1)![0]
    expect(callArgs.scope).toBe('mine')
  })

  it('loads the detail panel when a card is clicked, opens on double-click', async () => {
    list.mockResolvedValue({items: [flow('a', {name: 'Pipeline'})], total: 1, offset: 0, limit: 24})
    get.mockResolvedValue({...flow('a', {name: 'Pipeline'}), healthScore: 88, errorCount: 0, warningCount: 1})
    versions.mockResolvedValue([])
    getContent.mockResolvedValue({id: 'doc-a', name: 'Pipeline', subflows: []})
    renderWorkspace()

    const card = await screen.findByText('Pipeline')
    fireEvent.click(card)
    expect(await screen.findByText(/Last updated/)).toBeInTheDocument()
    expect(await screen.findByText('88%')).toBeInTheDocument()

    await act(async () => {
      fireEvent.doubleClick(card)
    })
    await waitFor(() => {
      expect(useFlowStore.getState().libraryFlowId).toBe('a')
      expect(useUIStore.getState().mainPaneView).toBe('block')
    })
  })

  it('renames a flow through the update endpoint', async () => {
    list.mockResolvedValue({items: [flow('a', {name: 'Pipeline'})], total: 1, offset: 0, limit: 24})
    get.mockResolvedValue(flow('a', {name: 'Pipeline'}))
    versions.mockResolvedValue([])
    update.mockResolvedValue({...flow('a', {name: 'Renamed'})})
    renderWorkspace()

    fireEvent.click(await screen.findByText('Pipeline'))
    fireEvent.click(await screen.findByRole('button', {name: /rename/i}))
    const input = await screen.findByDisplayValue('Pipeline')
    fireEvent.change(input, {target: {value: 'Renamed'}})
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', {name: 'Rename'}))
    await waitFor(() => expect(update).toHaveBeenCalledWith('a', {name: 'Renamed', version: 1}))
  })

  it('duplicates a flow as "<name> (copy)" in the same org', async () => {
    const orgFlow = flow('a', {name: 'Pipeline', orgId: 'org-9'})
    list.mockResolvedValue({items: [orgFlow], total: 1, offset: 0, limit: 24})
    get.mockResolvedValue(orgFlow)
    versions.mockResolvedValue([])
    getContent.mockResolvedValue({id: 'doc-a', name: 'Pipeline', subflows: []})
    create.mockResolvedValue(flow('b', {name: 'Pipeline (copy)', orgId: 'org-9'}))
    renderWorkspace()

    fireEvent.click(await screen.findByText('Pipeline'))
    fireEvent.click(await screen.findByRole('button', {name: /duplicate/i}))
    await waitFor(() =>
      expect(create).toHaveBeenCalledWith({
        name: 'Pipeline (copy)',
        orgId: 'org-9',
        content: expect.objectContaining({id: 'doc-a'}),
      }),
    )
    expect(await screen.findByText('Pipeline (copy)')).toBeInTheDocument()
  })

  it('edits tags through the detail panel', async () => {
    list.mockResolvedValue({items: [flow('a', {name: 'Pipeline'})], total: 1, offset: 0, limit: 24})
    get.mockResolvedValue({...flow('a', {name: 'Pipeline'}), tags: ['prod']})
    versions.mockResolvedValue([])
    setTags.mockResolvedValue({tags: ['prod', 'finance']})
    renderWorkspace()

    fireEvent.click(await screen.findByText('Pipeline'))
    expect(await screen.findByText('prod')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'Edit tags'}))
    const input = await screen.findByDisplayValue('prod')
    fireEvent.change(input, {target: {value: 'prod, finance'}})
    fireEvent.keyDown(input, {key: 'Enter'})

    await waitFor(() => expect(setTags).toHaveBeenCalledWith('a', ['prod', 'finance']))
  })

  it('offers the tags seen on flows as filter chips and filters by them', async () => {
    list.mockResolvedValue({
      items: [
        {...flow('a'), tags: ['prod']},
        {...flow('b'), tags: ['finance', 'prod']},
      ],
      total: 2,
      offset: 0,
      limit: 24,
    })
    renderWorkspace()
    const chip = await screen.findByRole('button', {name: 'finance'})
    expect(screen.getByRole('button', {name: 'prod'})).toHaveAttribute('aria-pressed', 'false')

    list.mockClear()
    fireEvent.click(chip)
    await waitFor(() => expect(list).toHaveBeenCalled())
    expect(list.mock.calls.at(-1)![0].tags).toEqual(['finance'])
    expect(await screen.findByRole('button', {name: 'finance'})).toHaveAttribute('aria-pressed', 'true')

    list.mockClear()
    fireEvent.click(screen.getByRole('button', {name: 'Clear'}))
    await waitFor(() => expect(list).toHaveBeenCalled())
    expect(list.mock.calls.at(-1)![0].tags).toBeUndefined()
  })

  it('turns a rename conflict into a reload prompt that re-reads the flow', async () => {
    list.mockResolvedValue({items: [flow('a', {name: 'Pipeline'})], total: 1, offset: 0, limit: 24})
    get.mockResolvedValue(flow('a', {name: 'Pipeline'}))
    versions.mockResolvedValue([])
    update.mockRejectedValue(new VersionConflictError('flow was modified concurrently; reload and retry'))
    renderWorkspace()

    fireEvent.click(await screen.findByText('Pipeline'))
    fireEvent.click(await screen.findByRole('button', {name: /rename/i}))
    fireEvent.change(await screen.findByDisplayValue('Pipeline'), {target: {value: 'Renamed'}})
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', {name: 'Rename'}))

    expect(await screen.findByText('Flow was modified by another user')).toBeInTheDocument()
    expect(screen.queryByText(/modified concurrently/)).toBeNull()

    get.mockClear()
    list.mockClear()
    fireEvent.click(screen.getByRole('button', {name: 'Reload'}))
    await waitFor(() => expect(get).toHaveBeenCalledWith('a'))
    expect(list).toHaveBeenCalled()
  })

  it('marks the detail pane open while a flow is selected, so it can slide over a narrow results pane', async () => {
    list.mockResolvedValue({items: [flow('a', {name: 'Pipeline'})], total: 1, offset: 0, limit: 24})
    get.mockResolvedValue({...flow('a', {name: 'Pipeline'})})
    versions.mockResolvedValue([])
    renderWorkspace()

    const card = await screen.findByText('Pipeline')
    const pane = screen.getByRole('complementary', {name: 'Flow details'})
    expect(pane).toHaveAttribute('data-open', 'false')
    fireEvent.click(card)
    expect(pane).toHaveAttribute('data-open', 'true')
    fireEvent.click(await screen.findByLabelText('Close detail panel'))
    expect(pane).toHaveAttribute('data-open', 'false')
  })

  it('switches between grid and list view', async () => {
    list.mockResolvedValue({items: [flow('a')], total: 1, offset: 0, limit: 24})
    renderWorkspace()
    await screen.findByText('Flow a')
    expect(useLibraryBrowseStore.getState().view).toBe('grid')
    fireEvent.click(screen.getByLabelText('List view'))
    expect(useLibraryBrowseStore.getState().view).toBe('list')
    expect(await screen.findByText('Updated')).toBeInTheDocument()
  })
})

describe('LibraryWorkspace paging', () => {
  const page = (ids: string[], extra: Record<string, unknown>) => ({
    items: ids.map(id => flow(id)),
    total: 50,
    offset: 0,
    limit: 24,
    ...extra,
  })

  it('reaches the next page by the cursor the page before answered, beside the offset', async () => {
    list.mockResolvedValueOnce(page(['a'], {hasMore: true, nextCursor: 'cur-1'}))
    list.mockResolvedValueOnce(page(['b'], {offset: 24, hasMore: true, nextCursor: 'cur-2'}))
    list.mockResolvedValue(page(['a'], {hasMore: true, nextCursor: 'cur-1'}))
    renderWorkspace()
    await screen.findByText('Flow a')
    expect(list.mock.calls[0]![0]).toMatchObject({offset: 0, cursor: undefined})

    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    await screen.findByText('Flow b')
    expect(list.mock.calls.at(-1)![0]).toMatchObject({offset: 24, cursor: 'cur-1'})

    // Back to the first page: no cursor; and a new filter starts over without the old cursors.
    fireEvent.click(screen.getByRole('button', {name: /Prev/}))
    await waitFor(() => expect(list.mock.calls.at(-1)![0]).toMatchObject({offset: 0, cursor: undefined}))
    await screen.findByText('Flow a')
    list.mockResolvedValue(page(['a'], {hasMore: true})) // a name sort pages by offset: no cursor
    act(() => useLibraryBrowseStore.getState().setSort('name_asc'))
    await waitFor(() => expect(list.mock.calls.at(-1)![0]).toMatchObject({sort: 'name_asc', offset: 0}))
    await screen.findByText('Flow a')
    act(() => useLibraryBrowseStore.getState().setPage(1))
    await waitFor(() => expect(list.mock.calls.at(-1)![0]).toMatchObject({sort: 'name_asc', offset: 24, cursor: undefined}))
  })

  it('pages by offset against a server that answers no cursor', async () => {
    list.mockResolvedValueOnce({items: [flow('a')], total: 50, offset: 0, limit: 24})
    list.mockResolvedValue({items: [flow('b')], total: 50, offset: 24, limit: 24})
    renderWorkspace()
    await screen.findByText('Flow a')
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    await screen.findByText('Flow b')
    expect(list.mock.calls.at(-1)![0]).toMatchObject({offset: 24, cursor: undefined})
    expect(screen.getByText('Page 2 of 3')).toBeInTheDocument()
  })

  it('shows a capped total as "more than", and lets hasMore decide whether a next page exists', async () => {
    list.mockResolvedValue(page(['a'], {total: 10000, totalCapped: true, hasMore: true, nextCursor: 'cur-1'}))
    renderWorkspace()
    await screen.findByText('Flow a')
    expect(screen.getAllByText('More than 10000 flows')).toHaveLength(2) // the toolbar's count and the pager's
    expect(screen.getByText('Page 1')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: /Next/})).toBeEnabled()

    list.mockResolvedValue(page(['z'], {offset: 9984, total: 10000, totalCapped: true, hasMore: false}))
    act(() => useLibraryBrowseStore.getState().setPage(416))
    await screen.findByText('Flow z')
    expect(screen.getByRole('button', {name: /Next/})).toBeDisabled()
  })

  it('stops paging by offset at the server cap, where only a cursor leads further', async () => {
    list.mockResolvedValue(page(['a'], {total: 10000, totalCapped: true, hasMore: true}))
    renderWorkspace()
    await screen.findByText('Flow a')
    // A name sort (no cursor) at the page whose next offset passes 10 000: the server would refuse it.
    list.mockResolvedValue(page(['y'], {offset: 9984, total: 10000, totalCapped: true, hasMore: true}))
    act(() => useLibraryBrowseStore.getState().setPage(416))
    await screen.findByText('Flow y')
    expect(screen.getByRole('button', {name: /Next/})).toBeDisabled()
    // With a cursor for the next page, it goes on.
    list.mockResolvedValue(
      page(['z'], {offset: 9960, total: 10000, totalCapped: true, hasMore: true, nextCursor: 'cur-416'}),
    )
    act(() => useLibraryBrowseStore.getState().setPage(415))
    await screen.findByText('Flow z')
    expect(screen.getByRole('button', {name: /Next/})).toBeEnabled()
  })

  it('starts over at the first page when a page past the cap has no cursor to reach it', async () => {
    list.mockResolvedValue({items: [], total: 0, offset: 0, limit: 24, hasMore: false})
    renderWorkspace()
    await waitFor(() => expect(list).toHaveBeenCalled())
    list.mockClear()
    act(() => useLibraryBrowseStore.getState().setPage(500))
    await waitFor(() => expect(useLibraryBrowseStore.getState().page).toBe(0))
    for (const call of list.mock.calls) expect(call[0].offset).toBeLessThanOrEqual(10000)
  })

  it('pages past the cap by offset against a server that predates the cursors (no cap there)', async () => {
    // The release before answers no hasMore and no cursor, and serves any offset.
    list.mockImplementation((q: {offset: number}) =>
      Promise.resolve({items: [flow(`p${q.offset / 24}`)], total: 12000, offset: q.offset, limit: 24}),
    )
    renderWorkspace()
    await screen.findByText('Flow p0')
    act(() => useLibraryBrowseStore.getState().setPage(416))
    await screen.findByText('Flow p416')
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    expect(await screen.findByText('Flow p417')).toBeInTheDocument()
    expect(useLibraryBrowseStore.getState().page).toBe(417)
    expect(list.mock.calls.at(-1)![0]).toMatchObject({offset: 10008, cursor: undefined})
  })

  it('waits for the page it asked for before Next leads on', async () => {
    list.mockImplementation((q: {offset: number}) =>
      Promise.resolve(
        page([`p${q.offset / 24}`], {offset: q.offset, hasMore: true, nextCursor: `c${q.offset / 24 + 1}`}),
      ),
    )
    renderWorkspace()
    await screen.findByText('Flow p0')
    let release: () => void = () => {}
    list.mockImplementationOnce(
      (q: {offset: number}) =>
        new Promise(
          resolve => (release = () => resolve(page(['p1'], {offset: q.offset, hasMore: true, nextCursor: 'c2'}))),
        ),
    )
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    // A second click now would reach page 3 before page 2 told its cursor.
    expect(screen.getByRole('button', {name: /Next/})).toBeDisabled()
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    act(() => release())
    await screen.findByText('Flow p1')
    expect(useLibraryBrowseStore.getState().page).toBe(1)
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    await screen.findByText('Flow p2')
    expect(list.mock.calls.at(-1)![0]).toMatchObject({offset: 48, cursor: 'c2'})
  })

  it('counts no fewer pages than it shows when a cursor carries an older total, and Refresh counts again', async () => {
    // Page 1 counted 30 flows; 40 were added since: the cursors carry the 30.
    list.mockImplementation((q: {offset: number; cursor?: string; limit: number}) => {
      if (q.limit === 1) return Promise.resolve(page(['n'], {total: 70, hasMore: true}))
      const n = q.offset / 24
      return Promise.resolve(
        page([`p${n}`], {offset: q.offset, total: 30, hasMore: n < 2, nextCursor: n < 2 ? `c${n + 1}` : undefined}),
      )
    })
    renderWorkspace()
    await screen.findByText('Flow p0')
    expect(screen.getByText('Page 1 of 2')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    await screen.findByText('Flow p1')
    expect(screen.getByText('Page 2 of 3')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: /Next/})).toBeEnabled()
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    await screen.findByText('Flow p2')
    expect(screen.getByText('Page 3 of 3')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: /Refresh/}))
    await waitFor(() =>
      expect(list).toHaveBeenCalledWith(expect.objectContaining({limit: 1, offset: 0}), expect.anything()),
    )
    expect(await screen.findByText('70 flows')).toBeInTheDocument()
    expect(screen.getByText('Page 3 of 3')).toBeInTheDocument()
  })

  it('keeps the pager on a page past the first whatever the count comes to', async () => {
    list.mockResolvedValueOnce(page(['a'], {total: 25, hasMore: true, nextCursor: 'cur-1'}))
    list.mockResolvedValue(page(['b'], {offset: 24, total: 25, hasMore: false}))
    get.mockResolvedValue(flow('b'))
    versions.mockResolvedValue([])
    renderWorkspace()
    await screen.findByText('Flow a')
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    fireEvent.click(await screen.findByText('Flow b'))
    fireEvent.click(await screen.findByRole('button', {name: 'Delete'}))
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', {name: 'Delete'}))
    // 24 flows fit one page, but this is the second: the way back stays.
    expect(await screen.findByText('24 flows')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: /Prev/})).toBeEnabled()
  })

  it('keeps the pager on the first page when a delete brings the count within it but a page still follows', async () => {
    list.mockResolvedValueOnce(page(['a', 'b'], {total: 25, hasMore: true, nextCursor: 'cur-1'}))
    list.mockResolvedValue(page(['y'], {offset: 24, total: 25, hasMore: false}))
    get.mockResolvedValue(flow('a'))
    versions.mockResolvedValue([])
    renderWorkspace()
    fireEvent.click(await screen.findByText('Flow a'))
    fireEvent.click(await screen.findByRole('button', {name: 'Delete'}))
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', {name: 'Delete'}))
    // 24 flows fit one page, but the page shown was made with 25: the one after it still holds the last flow.
    expect(await screen.findByText('24 flows')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    await screen.findByText('Flow y')
    expect(list.mock.calls.at(-1)![0]).toMatchObject({offset: 24, cursor: 'cur-1'})
  })

  it('takes the count of a server that predates the cursors on a page reached by cursor', async () => {
    // A rolling deploy: new replicas answer up to page 417 with cursors and a capped count; the release before
    // answers page 418 with its own full count and no cursor, and serves any offset.
    list.mockImplementation((q: {offset: number}) => {
      const n = q.offset / 24
      if (n === 418) return Promise.resolve({items: [flow(`p${n}`)], total: 12000, offset: q.offset, limit: 24})
      return Promise.resolve(
        page([`p${n}`], {offset: q.offset, total: 10000, totalCapped: true, hasMore: true, nextCursor: `c${n + 1}`}),
      )
    })
    renderWorkspace()
    await screen.findByText('Flow p0')
    act(() => useLibraryBrowseStore.getState().setPage(416))
    await screen.findByText('Flow p416')
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    await screen.findByText('Flow p417')
    await waitFor(() => expect(screen.getByRole('button', {name: /Next/})).toBeEnabled())
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    await screen.findByText('Flow p418')
    expect(list.mock.calls.at(-1)![0]).toMatchObject({offset: 10032, cursor: 'c418'})
    await waitFor(() => expect(screen.getByRole('button', {name: /Next/})).toBeEnabled())
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    await waitFor(() => expect(list.mock.calls.at(-1)![0]).toMatchObject({offset: 10056, cursor: undefined}))
    expect(useLibraryBrowseStore.getState().page).toBe(419)
  })

  it('keeps a delete on the count across pages reached by cursor', async () => {
    list.mockImplementation((q: {offset: number}) => {
      const n = q.offset / 24
      return Promise.resolve(page([`p${n}`], {offset: q.offset, total: 50, hasMore: true, nextCursor: `c${n + 1}`}))
    })
    get.mockResolvedValue(flow('p1'))
    versions.mockResolvedValue([])
    renderWorkspace()
    await screen.findByText('Flow p0')
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    fireEvent.click(await screen.findByText('Flow p1'))
    fireEvent.click(await screen.findByRole('button', {name: 'Delete'}))
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', {name: 'Delete'}))
    expect(await screen.findByText('49 flows')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: /Next/}))
    await screen.findByText('Flow p2')
    expect(screen.getByText('49 flows')).toBeInTheDocument() // the cursor carries the 50 counted before the delete
  })
})
