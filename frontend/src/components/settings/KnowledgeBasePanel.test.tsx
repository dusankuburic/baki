import {describe, it, expect, vi, beforeEach, onTestFinished} from 'vitest'
import {render, screen, fireEvent, waitFor, within} from '@testing-library/react'
import KnowledgeBasePanel from './KnowledgeBasePanel'
import {useOrgStore, type Organisation} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {useAzureFoundryStore} from '@/stores/azureFoundryStore'
import {ToastProvider, ConfirmProvider} from '@/components/shared'
import {ApiError} from '@/api/client'

const requestMock = vi.fn()

vi.mock('@/api/client', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    request: (...a: unknown[]) => requestMock(...a),
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

  // R-config-8: the listing names both widths and which side is off, since the two need opposite remedies —
  // an embedder off PAD_EMBEDDING_DIM is fixed in the environment, stored chunks off it by a re-index alone.
  it('tells an embedder off PAD_EMBEDDING_DIM from stored chunks off it', async () => {
    seedStore('admin')
    requestMock.mockResolvedValue([
      {
        id: 'd1',
        filename: 'wide.md',
        createdAt: new Date().toISOString(),
        chunkCount: 5,
        vectorIndexed: 0,
        embeddingWidthMismatch: {configured: 1536, observed: 3072, source: 'embedder'},
      },
      {
        id: 'd2',
        filename: 'old.md',
        createdAt: new Date().toISOString(),
        chunkCount: 5,
        vectorIndexed: 0,
        embeddingWidthMismatch: {configured: 1536, observed: 768, source: 'stored'},
      },
    ])
    renderPanel()

    expect(await screen.findByText('wide.md')).toBeInTheDocument()
    expect(
      screen.getByText(/produces 3072-dimensional vectors but PAD_EMBEDDING_DIM is 1536.*restart, and re-index/),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/stored embeddings are 768-dimensional but PAD_EMBEDDING_DIM is 1536; re-index below/),
    ).toBeInTheDocument()
    expect(screen.queryByText(/stored embeddings.*restart/)).not.toBeInTheDocument()
    expect(screen.queryByText(/dimension mismatch; re-index/i)).not.toBeInTheDocument()
  })

  // R-config-8: without a vector index no document is "not searchable" for being outside it — searches run
  // in-process — so the stranded line stays out although nothing is indexed.
  it('does not call documents unsearchable on a deployment without a vector index', async () => {
    seedStore('admin')
    requestMock.mockResolvedValue([
      {id: 'd1', filename: 'guide.md', createdAt: new Date().toISOString(), chunkCount: 5, noVectorIndex: true},
    ])
    renderPanel()

    expect(await screen.findByText('guide.md')).toBeInTheDocument()
    expect(screen.getByText(/5 chunks/)).toBeInTheDocument()
    expect(screen.queryByText(/not searchable/i)).not.toBeInTheDocument()
  })

  it('marks documents indexed with another embedding model', async () => {
    seedStore('admin')
    requestMock.mockResolvedValue([
      {
        id: 'd1',
        filename: 'other.md',
        createdAt: new Date().toISOString(),
        chunkCount: 4,
        vectorIndexed: 4,
        embeddingStranded: 4,
      },
      {id: 'd2', filename: 'mine.md', createdAt: new Date().toISOString(), chunkCount: 2, vectorIndexed: 2},
    ])
    renderPanel()

    expect(await screen.findByText('other.md')).toBeInTheDocument()
    expect(screen.getAllByText(/another embedding model; re-index/i)).toHaveLength(1)
    expect(screen.queryByText(/dimension mismatch/i)).not.toBeInTheDocument()
  })

  it('advertises the 1MB backend cap, not the flow-parser size limit', async () => {
    seedStore('admin')
    renderPanel()
    expect(await screen.findByText(/up to 1MB/i)).toBeInTheDocument()
  })

  it('tells admins that uploads are masked for secret patterns before embedding and storage', async () => {
    seedStore('admin')
    renderPanel()
    expect(
      await screen.findByText(/masked for secret patterns before they are embedded or stored/i),
    ).toBeInTheDocument()
  })

  it('re-indexes after confirmation and reports the outcome', async () => {
    seedStore('admin')
    requestMock.mockResolvedValue([
      {id: 'd1', filename: 'guide.md', createdAt: new Date().toISOString(), chunkCount: 2, vectorIndexed: 2},
    ])
    renderPanel()
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

  function routeRequests(handlers: {
    reindex?: (body: unknown, signal?: AbortSignal) => unknown
    upload?: () => unknown
  }) {
    requestMock.mockImplementation((url: string, opts?: {body?: unknown; signal?: AbortSignal}) => {
      if (url.endsWith('/knowledge/reindex') && handlers.reindex) return handlers.reindex(opts?.body, opts?.signal)
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
    expect((reindexCalls[0]![1] as {body: unknown}).body).toEqual({})
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

  it("asks before forcing when the embedder is not PAD_EMBEDDING_DIM wide, with the server's reason", async () => {
    seedStore('admin')
    const reason =
      "your embedding provider's vectors are not PAD_EMBEDDING_DIM wide, so a re-index cannot make them searchable: PAD_EMBEDDING_DIM is 2 and the provider's vectors are 3 wide — set PAD_EMBEDDING_DIM to 3 and restart, then re-index"
    routeRequests({
      reindex: body =>
        (body as {force?: boolean}).force
          ? Promise.resolve({chunks: 2, docs: 1})
          : Promise.reject(new ApiError(reason, 409, 'EMBEDDING_WIDTH_OFF')),
    })
    renderPanel()
    await confirmFirstReindexDialog()

    await screen.findByText(/re-index at the provider's width/i)
    const second = screen.getByRole('dialog')
    expect(within(second).getByText(/set PAD_EMBEDDING_DIM to 3 and restart/)).toBeInTheDocument()
    expect(within(second).getByText(/stay outside the vector index/)).toBeInTheDocument()
    expect(screen.queryByText('Re-index failed')).toBeNull()
    expect(screen.queryByText(/embedding provider/i, {selector: '[role="status"], [role="alert"]'})).toBeNull()
    fireEvent.click(within(second).getByRole('button', {name: 'Re-index anyway'}))

    await waitFor(() =>
      expect(requestMock).toHaveBeenCalledWith(
        '/api/orgs/org-1/knowledge/reindex',
        expect.objectContaining({body: {force: true}}),
      ),
    )
    const reindexCalls = requestMock.mock.calls.filter(c => String(c[0]).endsWith('/reindex'))
    expect(reindexCalls.map(c => (c[1] as {body: unknown}).body)).toEqual([{}, {force: true}])
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
    expect(await screen.findByText('embedding rate limit hit; retry in 20s')).toBeInTheDocument()
    expect(screen.queryByText(/Indexing requires an embedding-capable provider key/)).toBeNull()

    routeRequests({
      upload: () => Promise.reject(new ApiError('upstream unavailable', 503, 'EMBEDDING_PROVIDER_UNAVAILABLE')),
    })
    fireEvent.click(screen.getByText('Index Document'))
    expect(await screen.findByText('The embedding provider is unavailable — try again shortly')).toBeInTheDocument()
    expect(screen.queryByText('upstream unavailable')).toBeNull()
  })

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

  it('follows nextCursor across pages and reports the total', async () => {
    seedStore('admin')
    const pages = [
      {chunks: 4000, docs: 3, nextCursor: {docId: 'd2', ordinal: 7}},
      {chunks: 4000, docs: 2, nextCursor: {docId: 'd5', ordinal: 1}},
      {chunks: 1500, docs: 1},
    ]
    let i = 0
    routeRequests({reindex: () => Promise.resolve(pages[i++])})
    renderPanel()
    await confirmFirstReindexDialog()

    expect(await screen.findByText('Re-indexed 9500 chunks')).toBeInTheDocument()
    const bodies = requestMock.mock.calls
      .filter(c => String(c[0]).endsWith('/reindex'))
      .map(c => (c[1] as {body: unknown}).body)
    expect(bodies).toEqual([{}, {cursor: {docId: 'd2', ordinal: 7}}, {cursor: {docId: 'd5', ordinal: 1}}])
  })

  it('keeps force on every page after the model-change confirmation', async () => {
    seedStore('admin')
    routeRequests({
      reindex: body => {
        const b = body as {force?: boolean; cursor?: unknown}
        if (!b.force) return modelChanged()
        return Promise.resolve(b.cursor ? {chunks: 1, docs: 1} : {chunks: 2, docs: 1, nextCursor: {docId: 'd1'}})
      },
    })
    renderPanel()
    await confirmFirstReindexDialog()
    await screen.findByText(/switch the embedding model/i)
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', {name: 'Re-index with my model'}))

    expect(await screen.findByText('Re-indexed 3 chunks')).toBeInTheDocument()
    const bodies = requestMock.mock.calls
      .filter(c => String(c[0]).endsWith('/reindex'))
      .map(c => (c[1] as {body: unknown}).body)
    expect(bodies).toEqual([{}, {force: true}, {force: true, cursor: {docId: 'd1'}}])
  })

  it('shows progress between pages and stops on request', async () => {
    seedStore('admin')
    let call = 0
    routeRequests({
      reindex: (_body, signal) => {
        call++
        if (call === 1) return Promise.resolve({chunks: 250, docs: 2, nextCursor: {docId: 'd2'}})
        return new Promise((_resolve, reject) => {
          signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')))
        })
      },
    })
    renderPanel()
    await confirmFirstReindexDialog()

    expect(await screen.findByRole('status')).toHaveTextContent('Re-indexed 250 chunks so far…')
    fireEvent.click(screen.getByRole('button', {name: 'Stop'}))

    expect(await screen.findByText(/Re-index stopped after 250 chunks/)).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByRole('button', {name: 'Stop'})).toBeNull())
    expect(screen.queryByText('Re-index failed')).toBeNull()
  })

  it('explains a server timeout before any page committed', async () => {
    seedStore('admin')
    routeRequests({reindex: () => Promise.reject(new ApiError('context deadline exceeded', 504, 'KNOWLEDGE_TIMEOUT'))})
    renderPanel()
    await confirmFirstReindexDialog()
    expect(
      await screen.findByText('The server ran out of time before anything was indexed — try again'),
    ).toBeInTheDocument()
  })

  it('explains a model change refused on a later page instead of a bare failure', async () => {
    seedStore('admin')
    let call = 0
    routeRequests({
      reindex: () =>
        ++call === 1
          ? Promise.resolve({chunks: 500, docs: 1, nextCursor: {docId: 'd1', id: 'c500'}})
          : Promise.reject(new ApiError('embedding model changed', 409, 'EMBEDDING_MODEL_CHANGED')),
    })
    renderPanel()
    await confirmFirstReindexDialog()
    expect(await screen.findByText(/Your embedding model changed while the re-index was running/)).toBeInTheDocument()
    expect(call).toBe(2)
  })
  it('offers Resume after Stop and continues from the committed page', async () => {
    seedStore('admin')
    let call = 0
    routeRequests({
      reindex: (_body, signal) => {
        call++
        if (call === 1) return Promise.resolve({chunks: 250, docs: 2, nextCursor: {docId: 'd2'}})
        return new Promise((_resolve, reject) => {
          signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')))
        })
      },
    })
    renderPanel()
    await confirmFirstReindexDialog()

    expect(await screen.findByRole('status')).toHaveTextContent('Re-indexed 250 chunks so far…')
    fireEvent.click(screen.getByRole('button', {name: 'Stop'}))
    expect(await screen.findByText(/The pages done are kept/)).toBeInTheDocument()

    routeRequests({reindex: () => Promise.resolve({chunks: 30, docs: 1})})
    fireEvent.click(await screen.findByRole('button', {name: 'Resume re-index'}))
    await waitFor(() =>
      expect(requestMock).toHaveBeenCalledWith(
        '/api/orgs/org-1/knowledge/reindex',
        expect.objectContaining({body: {cursor: {docId: 'd2'}}}),
      ),
    )
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(await screen.findByText('Re-indexed 30 chunks across 1 documents')).toBeInTheDocument()
    expect(await waitFor(() => screen.queryByRole('button', {name: 'Resume re-index'}))).toBeNull()
  })

  it('says to run Re-index again when stopped before the first page', async () => {
    seedStore('admin')
    routeRequests({
      reindex: (_body, signal) =>
        new Promise((_resolve, reject) => {
          signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')))
        }),
    })
    renderPanel()
    await confirmFirstReindexDialog()

    fireEvent.click(await screen.findByRole('button', {name: 'Stop'}))
    expect(
      await screen.findByText('Re-index stopped after 0 chunks. Run Re-index again to redo the rest.'),
    ).toBeInTheDocument()
    expect(screen.queryByText(/Resume continues/)).toBeNull()
    await waitFor(() => expect(screen.queryByRole('button', {name: 'Stop'})).toBeNull())
    expect(screen.queryByRole('button', {name: 'Resume re-index'})).toBeNull()
  })

  it('explains a model change refused on the first page of a resumed run and drops Resume', async () => {
    seedStore('admin')
    routeRequests({
      reindex: () =>
        Promise.reject(
          new ApiError('embedding rate limit hit; retry in 20s', 429, 'EMBEDDING_RATE_LIMITED', null, {
            nextCursor: {docId: 'd9', id: 'c700'},
            chunks: 700,
          }),
        ),
    })
    renderPanel()
    await confirmFirstReindexDialog()
    expect(await screen.findByText('embedding rate limit hit; retry in 20s')).toBeInTheDocument()

    routeRequests({reindex: () => modelChanged()})
    fireEvent.click(await screen.findByRole('button', {name: 'Resume re-index'}))
    expect(await screen.findByText(/Your embedding model changed while the re-index was running/)).toBeInTheDocument()
    expect(screen.queryByText('Re-index failed')).toBeNull()
    await waitFor(() => expect(screen.queryByRole('button', {name: 'Stop'})).toBeNull())
    expect(screen.queryByRole('button', {name: 'Resume re-index'})).toBeNull()
  })

  it('resumes a failed page from the error details cursor', async () => {
    seedStore('admin')
    routeRequests({
      reindex: () =>
        Promise.reject(
          new ApiError('embedding rate limit hit; retry in 20s', 429, 'EMBEDDING_RATE_LIMITED', null, {
            nextCursor: {docId: 'd9', id: 'c700'},
            chunks: 700,
          }),
        ),
    })
    renderPanel()
    await confirmFirstReindexDialog()

    expect(await screen.findByText('embedding rate limit hit; retry in 20s')).toBeInTheDocument()
    routeRequests({reindex: () => Promise.resolve({chunks: 5, docs: 1})})
    fireEvent.click(await screen.findByRole('button', {name: 'Resume re-index'}))
    await waitFor(() =>
      expect(requestMock).toHaveBeenCalledWith(
        '/api/orgs/org-1/knowledge/reindex',
        expect.objectContaining({body: {cursor: {docId: 'd9', id: 'c700'}}}),
      ),
    )
  })

  it('shows the document-too-large verdict verbatim on upload', async () => {
    seedStore('admin')
    routeRequests({
      upload: () =>
        Promise.reject(
          new ApiError(
            'document too large to index: 3000 passages exceed the limit of 2500',
            413,
            'KNOWLEDGE_DOCUMENT_TOO_LARGE',
          ),
        ),
    })
    const {container} = renderPanel()
    await screen.findByText('guide.md')
    const input = container.querySelector('input[type="file"]') as HTMLInputElement
    fireEvent.change(input, {target: {files: [new File(['# rules'], 'rules.md', {type: 'text/markdown'})]}})
    fireEvent.click(screen.getByText('Index Document'))
    expect(await screen.findByText(/3000 passages exceed the limit of 2500/)).toBeInTheDocument()
  })

  it("shows the org's not-configured verdict verbatim on re-index", async () => {
    seedStore('admin')
    routeRequests({
      reindex: () =>
        Promise.reject(
          new ApiError(
            "the organisation's shared Azure AI Foundry credential names no embedding deployment — ask an org admin to add one",
            400,
            'EMBEDDING_ORG_NOT_CONFIGURED',
          ),
        ),
    })
    renderPanel()
    await confirmFirstReindexDialog()
    expect(await screen.findByText(/ask an org admin to add one/)).toBeInTheDocument()
  })

  describe('after the knowledge-base model changed', () => {
    const NOTICE = /The knowledge base embedding model changed/

    function suggestReindex(keys: Record<string, true>) {
      useAzureFoundryStore.setState({knowledgeReindex: keys})
      onTestFinished(() => useAzureFoundryStore.setState({knowledgeReindex: {}}))
    }

    it('asks an admin to re-index, and stops asking once the re-index ran', async () => {
      seedStore('admin')
      suggestReindex({'org:org-1': true})
      routeRequests({reindex: () => Promise.resolve({chunks: 2, docs: 1})})
      renderPanel()
      const notice = (await screen.findByText(NOTICE)).parentElement!
      expect(notice).toHaveTextContent('uploads are refused until then')
      fireEvent.click(within(notice).getByRole('button', {name: 'Re-index'}))
      const dialog = await screen.findByRole('dialog')
      fireEvent.click(within(dialog).getByRole('button', {name: 'Re-index'}))
      await waitFor(() => expect(screen.queryByText(NOTICE)).toBeNull())
      expect(useAzureFoundryStore.getState().knowledgeReindex).toEqual({})
    })

    it('keeps asking after a re-index that left documents waiting', async () => {
      seedStore('admin')
      suggestReindex({user: true})
      routeRequests({reindex: () => Promise.resolve({chunks: 10000, docs: 4, truncated: true})})
      renderPanel()
      const notice = (await screen.findByText(NOTICE)).parentElement!
      fireEvent.click(within(notice).getByRole('button', {name: 'Re-index'}))
      fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', {name: 'Re-index'}))
      expect(await screen.findByText(/more remain; run Re-index again to continue/)).toBeInTheDocument()
      expect(screen.getByText(NOTICE)).toBeInTheDocument()
    })

    it('tells a member that an admin has to re-index', async () => {
      seedStore('member')
      suggestReindex({'org:org-1': true})
      routeRequests({})
      renderPanel()
      expect(await screen.findByText(/An organisation admin needs to re-index it/)).toBeInTheDocument()
      expect(screen.queryByRole('button', {name: 'Re-index'})).toBeNull()
    })

    it("says nothing for another organisation's change or an empty knowledge base", async () => {
      seedStore('admin')
      suggestReindex({'org:org-2': true})
      routeRequests({})
      const {unmount} = renderPanel()
      await screen.findByText('guide.md')
      expect(screen.queryByText(NOTICE)).toBeNull()
      unmount()

      useAzureFoundryStore.setState({knowledgeReindex: {'org:org-1': true}})
      requestMock.mockResolvedValue([])
      renderPanel()
      await screen.findByText('No documents indexed yet.')
      expect(screen.queryByText(NOTICE)).toBeNull()
    })
  })
})
