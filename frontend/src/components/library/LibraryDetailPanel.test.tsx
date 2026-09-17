import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import LibraryDetailPanel from './LibraryDetailPanel'

const get = vi.fn()
const versions = vi.fn()

vi.mock('@/api/library', async () => {
  const actual = await vi.importActual<typeof import('@/api/library')>('@/api/library')
  return {
    ...actual,
    libraryApi: {
      get: (...a: unknown[]) => get(...a),
      versions: (...a: unknown[]) => versions(...a),
    },
  }
})

const noop = () => {}
function renderPanel() {
  return render(
    <LibraryDetailPanel
      flowId="f1"
      onOpen={noop}
      onDelete={noop}
      onRename={noop}
      onDuplicate={noop}
      onSaveTags={noop}
      editingTags={false}
      setEditingTags={noop}
      onClose={noop}
    />,
  )
}

const flow = {
  id: 'f1',
  name: 'Flow one',
  ownerId: 'me',
  organizationId: '',
  blockCount: 1,
  subflowCount: 1,
  updatedAt: '2026-01-01T00:00:00Z',
  version: 1,
}

const tauriWindow = window as unknown as Record<string, unknown>

describe('LibraryDetailPanel version history', () => {
  beforeEach(() => {
    get.mockResolvedValue(flow)
  })
  afterEach(() => {
    vi.clearAllMocks()
    delete tauriWindow.__TAURI__
  })

  // The versions call used to be `.catch(() => [])`, so a 503 ("versioning
  // requires a storage backend") rendered "No saved versions yet." — an
  // empty list where the feature does not exist. The user was told their
  // history was empty, not that it was unavailable.
  it('shows an error state when the versions call fails, not an empty list', async () => {
    versions.mockRejectedValue(new Error('versioning requires a storage backend (cloud mode)'))
    renderPanel()
    expect(await screen.findByText(/Version history unavailable/)).toBeInTheDocument()
    expect(screen.queryByText('No saved versions yet.')).toBeNull()
  })

  it('hides version history entirely in desktop (local) mode', async () => {
    tauriWindow.__TAURI__ = {}
    versions.mockRejectedValue(new Error('503'))
    renderPanel()
    await screen.findByText('Flow one')
    expect(screen.queryByText(/Version history/)).toBeNull()
    expect(versions).not.toHaveBeenCalled()
  })

  it('still renders the empty state for a genuinely empty history', async () => {
    versions.mockResolvedValue([])
    renderPanel()
    expect(await screen.findByText('No saved versions yet.')).toBeInTheDocument()
  })
})

describe('LibraryDetailPanel actions', () => {
  afterEach(() => vi.clearAllMocks())

  it('offers rename and duplicate only to editors', async () => {
    versions.mockResolvedValue([])
    get.mockResolvedValue({...flow, canEdit: false, canDelete: false})
    const {unmount} = renderPanel()
    await screen.findByText('Flow one')
    expect(screen.queryByRole('button', {name: /rename/i})).toBeNull()
    expect(screen.queryByRole('button', {name: /duplicate/i})).toBeNull()
    // Viewers can still load the flow read-only.
    expect(screen.getByRole('button', {name: 'Load flow'})).toBeInTheDocument()
    unmount()

    get.mockResolvedValue({...flow, canEdit: true, canDelete: true})
    renderPanel()
    expect(await screen.findByRole('button', {name: /rename/i})).toBeInTheDocument()
    expect(screen.getByRole('button', {name: /duplicate/i})).toBeInTheDocument()
  })
})
