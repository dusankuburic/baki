import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, act, within} from '@testing-library/react'
import LibraryWorkspace from './LibraryWorkspace'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {useLibraryBrowseStore} from '@/stores/libraryBrowseStore'
import {useOrgStore} from '@/stores/orgStore'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'

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
  // Reset stores between tests so leftover state doesn't bleed across cases.
  useLibraryBrowseStore.getState().reset()
  useOrgStore.setState({organisations: [], activeOrgId: null, isLoading: false, isBusy: false, error: null})
  useFlowStore.setState({document: null, libraryFlowId: null, libraryVersion: 0})
  useUIStore.setState({mainPaneView: 'library'})
  list.mockResolvedValue({items: [], total: 0, offset: 0, limit: 24})
})

describe('LibraryWorkspace', () => {
  it('shows skeleton while loading and the empty state when the list is empty', async () => {
    renderWorkspace()
    // Skeleton renders synchronously before the awaited list resolves.
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

    // Double-click opens the flow (hydrates flowStore + switches to block view).
    await act(async () => {
      fireEvent.doubleClick(card)
    })
    await waitFor(() => {
      expect(useFlowStore.getState().libraryFlowId).toBe('a')
      expect(useUIStore.getState().mainPaneView).toBe('block')
    })
  })

  // R0-4: rename + duplicate from the detail panel — the update endpoint
  // always accepted {name}; no UI ever sent it.
  it('renames a flow through the update endpoint', async () => {
    list.mockResolvedValue({items: [flow('a', {name: 'Pipeline'})], total: 1, offset: 0, limit: 24})
    get.mockResolvedValue(flow('a', {name: 'Pipeline'}))
    versions.mockResolvedValue([])
    update.mockResolvedValue({...flow('a', {name: 'Renamed'})})
    renderWorkspace()

    fireEvent.click(await screen.findByText('Pipeline'))
    fireEvent.click(await screen.findByRole('button', {name: /rename/i}))
    // The prompt dialog: type a new name, confirm (the dialog's confirm
    // button is scoped to the dialog to avoid matching the toolbar button).
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
    // The copy appears in the list without a refetch.
    expect(await screen.findByText('Pipeline (copy)')).toBeInTheDocument()
  })

  // R2-4b: tag editing — chips render from the list payload; the inline
  // editor saves through flowApi.setTags and updates the visible item.
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

  it('switches between grid and list view', async () => {
    list.mockResolvedValue({items: [flow('a')], total: 1, offset: 0, limit: 24})
    renderWorkspace()
    await screen.findByText('Flow a')
    expect(useLibraryBrowseStore.getState().view).toBe('grid')
    fireEvent.click(screen.getByLabelText('List view'))
    expect(useLibraryBrowseStore.getState().view).toBe('list')
    // The list-view header shows "Updated" while the grid card doesn't.
    expect(await screen.findByText('Updated')).toBeInTheDocument()
  })
})
