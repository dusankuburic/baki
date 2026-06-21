import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, act} from '@testing-library/react'
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
      create: vi.fn(),
      update: vi.fn(),
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
