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
    requestMock.mockResolvedValue([
      {id: 'd1', filename: 'guide.md', createdAt: new Date().toISOString(), chunkCount: 3, vectorIndexed: 3},
    ])
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
    requestMock.mockResolvedValue([
      {id: 'd1', filename: 'old.md', createdAt: new Date().toISOString(), chunkCount: 5, vectorIndexed: 0},
    ])
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
    requestMock.mockResolvedValue([
      {id: 'd1', filename: 'guide.md', createdAt: new Date().toISOString(), chunkCount: 2, vectorIndexed: 2},
    ])
    renderPanel()
    // Toolbar trigger first (exact label avoids also matching the dialog's
    // confirm button once it opens).
    const trigger = await screen.findByRole('button', {name: 'Re-index'})
    fireEvent.click(trigger)
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', {name: 'Re-index'}))

    await waitFor(() =>
      expect(requestMock).toHaveBeenCalledWith(
        '/api/orgs/org-1/knowledge/reindex',
        expect.objectContaining({body: {}}),
      ),
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
      expect(requestMock).toHaveBeenCalledWith(
        '/api/orgs/org-1/knowledge/reindex',
        expect.objectContaining({body: {}}),
      ),
    )
  })

  // Routes the mocked API by URL: the document list, then the re-index /
  // upload responses the case under test scripts.
  function routeRequests(handlers: {reindex?: (body: unknown) => unknown; upload?: () => unknown}) {
    requestMock.mockImplementation((url: string, opts?: {body?: unknown}) => {
      if (url.endsWith('/knowledge/reindex') && handlers.reindex) return handlers.reindex(opts?.body)
      if (url.endsWith('/knowledge/upload') && handlers.upload) return handlers.upload()
      return Promise.resolve([
        {id: 'd1', filename: 'guide.md', createdAt: new Date().toISOString(), chunkCount: 2, vectorIndexed: 2},
      ])
    })
  }

  const modelChanged = () =>
    Promise.reject(new ApiError('embedding model differs from the indexed corpus', 409, 'EMBEDDING_MODEL_CHANGED'))

  async function confirmFirstReindexDialog() {
    fireEvent.click(await screen.findByRole('button', {name: 'Re-index'}))
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', {name: 'Re-index'}))
  }

  // #35: re-indexing a corpus built with another embedding model strands every
  // member on that model, so the 409 turns into a second, explicit confirm
  // and only then is the request resent with force.
  it('asks again and resends with force when the embedding model changed', async () => {
    seedStore('admin')
    routeRequests({
      reindex: body => ((body as {force?: boolean}).force ? Promise.resolve({chunks: 2, docs: 1}) : modelChanged()),
    })
    renderPanel()
    await confirmFirstReindexDialog()

    await screen.findByText(/switch the embedding model/i)
    const second = screen.getByRole('dialog')
    expect(within(second).getByText(/members whose embedding provider uses another model/i)).toBeInTheDocument()
    fireEvent.click(within(second).getByRole('button', {name: 'Re-index with my model'}))

    await waitFor(() =>
      expect(requestMock).toHaveBeenCalledWith(
        '/api/orgs/org-1/knowledge/reindex',
        expect.objectContaining({body: {force: true}}),
      ),
    )
    const reindexCalls = requestMock.mock.calls.filter(c => String(c[0]).endsWith('/reindex'))
    expect(reindexCalls.map(c => (c[1] as {body: unknown}).body)).toEqual([{}, {force: true}])
  })

  it('does not force a re-index when the second confirm is cancelled', async () => {
    seedStore('admin')
    routeRequests({reindex: () => modelChanged()})
    renderPanel()
    await confirmFirstReindexDialog()

    await screen.findByText(/switch the embedding model/i)
    const second = screen.getByRole('dialog')
    fireEvent.click(within(second).getByRole('button', {name: 'Cancel'}))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    const reindexCalls = requestMock.mock.calls.filter(c => String(c[0]).endsWith('/reindex'))
    expect(reindexCalls).toHaveLength(1)
    expect((reindexCalls[0][1] as {body: unknown}).body).toEqual({})
  })

  it('explains an embedding model mismatch on upload', async () => {
    seedStore('admin')
    routeRequests({upload: () => modelChanged()})
    const {container} = renderPanel()
    await screen.findByText('guide.md')

    const input = container.querySelector('input[type="file"]') as HTMLInputElement
    fireEvent.change(input, {target: {files: [new File(['# rules'], 'rules.md', {type: 'text/markdown'})]}})
    fireEvent.click(screen.getByText('Index Document'))

    expect(
      await screen.findByText(/your embedding model differs from the one this knowledge base/i),
    ).toBeInTheDocument()
    expect(requestMock).toHaveBeenCalledWith(
      '/api/orgs/org-1/knowledge/upload',
      expect.objectContaining({body: {filename: 'rules.md', content: '# rules'}}),
    )
  })

  // Contract: `truncated` means the request hit its 10 000-chunk ceiling with
  // documents still waiting — a warning, not a success, since the admin has
  // to run it again.
  it('warns instead of celebrating when a re-index was truncated', async () => {
    seedStore('admin')
    routeRequests({reindex: () => Promise.resolve({chunks: 10000, docs: 4, truncated: true})})
    renderPanel()
    await confirmFirstReindexDialog()

    expect(
      await screen.findByText('Re-indexed 10000 chunks — more remain; run Re-index again to continue'),
    ).toBeInTheDocument()
    expect(screen.queryByText(/across 4 documents/)).toBeNull()
  })

  it('reports a complete re-index as a success', async () => {
    seedStore('admin')
    routeRequests({reindex: () => Promise.resolve({chunks: 7, docs: 2})})
    renderPanel()
    await confirmFirstReindexDialog()

    expect(await screen.findByText('Re-indexed 7 chunks across 2 documents')).toBeInTheDocument()
  })

  it("shows the server's message when another upload or re-index holds the corpus", async () => {
    seedStore('admin')
    routeRequests({
      reindex: () => Promise.reject(new ApiError('another re-index is already running', 409, 'KNOWLEDGE_BUSY')),
    })
    renderPanel()
    await confirmFirstReindexDialog()

    expect(await screen.findByText('another re-index is already running')).toBeInTheDocument()
    expect(screen.queryByText('Re-index failed')).toBeNull()
  })

  it("shows the embedding provider's own verdict on upload, and fixed copy when it is unavailable", async () => {
    seedStore('admin')
    routeRequests({
      upload: () =>
        Promise.reject(new ApiError('embedding rate limit hit; retry in 20s', 429, 'EMBEDDING_RATE_LIMITED')),
    })
    const {container} = renderPanel()
    await screen.findByText('guide.md')

    const input = container.querySelector('input[type="file"]') as HTMLInputElement
    fireEvent.change(input, {target: {files: [new File(['# rules'], 'rules.md', {type: 'text/markdown'})]}})
    fireEvent.click(screen.getByText('Index Document'))
    // The message mentions the embedding provider, yet it is not the
    // missing-key case: the verdict wins over the regex fallback.
    expect(await screen.findByText('embedding rate limit hit; retry in 20s')).toBeInTheDocument()
    expect(screen.queryByText(/Indexing requires an embedding-capable provider key/)).toBeNull()

    // 503: the server masks the transport detail, so the copy is ours.
    routeRequests({
      upload: () => Promise.reject(new ApiError('upstream unavailable', 503, 'EMBEDDING_PROVIDER_UNAVAILABLE')),
    })
    fireEvent.click(screen.getByText('Index Document'))
    expect(await screen.findByText('The embedding provider is unavailable — try again shortly')).toBeInTheDocument()
    expect(screen.queryByText('upstream unavailable')).toBeNull()
  })

  // The server gives both bulk calls a 5-minute deadline; the client's default
  // 30-second abort would cut a large corpus off first.
  it('waits at least five minutes on upload and re-index', async () => {
    seedStore('admin')
    routeRequests({reindex: () => Promise.resolve({chunks: 1, docs: 1}), upload: () => Promise.resolve({})})
    const {container} = renderPanel()
    await screen.findByText('guide.md')

    const input = container.querySelector('input[type="file"]') as HTMLInputElement
    fireEvent.change(input, {target: {files: [new File(['# rules'], 'rules.md', {type: 'text/markdown'})]}})
    fireEvent.click(screen.getByText('Index Document'))
    await waitFor(() => expect(requestMock).toHaveBeenCalledWith('/api/orgs/org-1/knowledge/upload', expect.anything()))
    await confirmFirstReindexDialog()
    await waitFor(() =>
      expect(requestMock).toHaveBeenCalledWith('/api/orgs/org-1/knowledge/reindex', expect.anything()),
    )
    for (const suffix of ['/knowledge/upload', '/knowledge/reindex']) {
      const call = requestMock.mock.calls.find(c => String(c[0]).endsWith(suffix))!
      expect((call[1] as {timeoutMs?: number}).timeoutMs).toBeGreaterThanOrEqual(5 * 60_000)
    }
  })
})
