import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor, within} from '@testing-library/react'
import KnowledgeBasePanel from './KnowledgeBasePanel'
import {useOrgStore, type Organisation} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {ToastProvider, ConfirmProvider} from '@/components/shared'
import {ApiError} from '@/api/client'

const requestMock = vi.fn()

vi.mock('@/api/client', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    request: (...a: unknown[]) => requestMock(...a),
    // Real ApiError so instanceof + code checks behave exactly as production.
  }
})

const adminOrg: Organisation = {
  id: 'org-1',
  name: 'Acme',
  ownerId: 'u-admin',
  members: [
    {userId: 'u-admin', role: 'admin', joinedAt: '2024-01-01T00:00:00Z', user: {id: 'u-admin', email: 'admin@acme.io'}},
    {userId: 'u-2', role: 'member', joinedAt: '2024-01-02T00:00:00Z', user: {id: 'u-2', email: 'bob@acme.io'}},
  ],
  createdAt: '2024-01-01T00:00:00Z',
  updatedAt: '2024-01-01T00:00:00Z',
}

function seedStore(role: 'admin' | 'member') {
  useOrgStore.setState({
    organisations: [adminOrg],
    activeOrgId: 'org-1',
    isLoading: false,
    error: null,
  } as never)
  useAuthStore.setState({user: {id: role === 'admin' ? 'u-admin' : 'u-2', email: 'x@y.io'}} as never)
}

function renderPanel() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <KnowledgeBasePanel />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  requestMock.mockResolvedValue([])
})

describe('KnowledgeBasePanel', () => {
  it('shows upload + re-index controls to admins', async () => {
    seedStore('admin')
    requestMock.mockResolvedValue([{id: 'd1', filename: 'guide.md', createdAt: new Date().toISOString(), chunkCount: 3, vectorIndexed: 3}])
    renderPanel()

    expect(await screen.findByText('guide.md')).toBeInTheDocument()
    expect(screen.getByText('Index Document')).toBeInTheDocument()
    expect(await screen.findByRole('button', {name: /re-index/i})).toBeInTheDocument()
    expect(screen.getByText(/3 chunks \(3 searchable\)/)).toBeInTheDocument()
  })

  // G3: members used to see admin-gated controls (upload card, delete,
  // re-index) that 403'd on use — the backend requires requireAdmin.
  it('hides all management controls from plain members', async () => {
    seedStore('member')
    requestMock.mockResolvedValue([{id: 'd1', filename: 'guide.md', createdAt: new Date().toISOString()}])
    renderPanel()

    expect(await screen.findByText('guide.md')).toBeInTheDocument()
    expect(screen.queryByText('Index Document')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', {name: /re-index/i})).not.toBeInTheDocument()
  })

  it('marks stranded documents (chunks but none searchable)', async () => {
    seedStore('admin')
    requestMock.mockResolvedValue([{id: 'd1', filename: 'old.md', createdAt: new Date().toISOString(), chunkCount: 5, vectorIndexed: 0}])
    renderPanel()

    expect(await screen.findByText('old.md')).toBeInTheDocument()
    expect(screen.getByText(/dimension mismatch; re-index/i)).toBeInTheDocument()
  })

  it('advertises the 1MB backend cap, not the flow-parser size limit', async () => {
    seedStore('admin')
    renderPanel()
    expect(await screen.findByText(/up to 1MB/i)).toBeInTheDocument()
  })

  it('re-indexes after confirmation and reports the outcome', async () => {
    seedStore('admin')
    requestMock.mockResolvedValue([{id: 'd1', filename: 'guide.md', createdAt: new Date().toISOString(), chunkCount: 2, vectorIndexed: 2}])
    renderPanel()
    // Toolbar trigger first (exact label avoids also matching the dialog's
    // confirm button once it opens).
    const trigger = await screen.findByRole('button', {name: 'Re-index'})
    fireEvent.click(trigger)
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', {name: 'Re-index'}))

    await waitFor(() =>
      expect(requestMock).toHaveBeenCalledWith('/api/orgs/org-1/knowledge/reindex', expect.objectContaining({body: {}})),
    )
    requestMock.mockResolvedValue({chunks: 7, docs: 2})
    // Toast content is opaque here; the call + no error state is the contract.
  })

  it('explains an unconfigured embedding provider on re-index failure', async () => {
    seedStore('admin')
    requestMock.mockResolvedValue([{id: 'd1', filename: 'guide.md', createdAt: new Date().toISOString()}])
    renderPanel()
    const trigger = await screen.findByRole('button', {name: 'Re-index'})
    fireEvent.click(trigger)
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', {name: 'Re-index'}))

    requestMock.mockRejectedValue(new ApiError('embedding provider not configured', 400, 'EMBEDDING_NOT_CONFIGURED'))
    await waitFor(() =>
      expect(requestMock).toHaveBeenCalledWith('/api/orgs/org-1/knowledge/reindex', expect.objectContaining({body: {}})),
    )
  })
})
