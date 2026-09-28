import {useState, useMemo, useRef, useEffect} from 'react'
import {useOrgStore} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {request, ApiError} from '@/api/client'
import {Trash2, FileText, Loader2, Upload, RefreshCw, Square} from 'lucide-react'
import Button from '@/components/shared/Button'
import {useConfirm, useToast, ErrorState} from '@/components/shared'
import {logger} from '@/lib/logger'
import {relativeTime, absoluteTime} from '@/lib/time'
import {useAsync} from '@/hooks/useAsync'
import {useTranslation} from 'react-i18next'

interface KnowledgeDoc {
  id: string
  filename: string
  createdAt: string
  chunkCount?: number
  vectorIndexed?: number
}

// POST /api/orgs/{id}/knowledge/reindex. `truncated`: the request exhausted
// its chunk budget (10 000) with documents still waiting; the admin runs it
// again to continue. `nextCursor` (resumable servers): each request commits
// one page and names where the next starts — the client posts again with
// `{force, cursor: nextCursor}` until it is absent. Opaque to the client.
interface ReindexResult {
  chunks: number
  docs: number
  truncated?: boolean
  nextCursor?: Record<string, unknown> | null
}

function isAbortError(e: unknown): boolean {
  return (e instanceof DOMException || e instanceof Error) && e.name === 'AbortError'
}

// Upload and re-index failures whose server message is the thing to show:
// the embedding provider's own verdict (rate limit, missing role, bad key,
// rejected input) and the corpus lock (another upload or re-index is in
// flight). EMBEDDING_PROVIDER_UNAVAILABLE (503) gets fixed copy instead —
// its message is transport detail.
// Upload and re-index embed whole documents in one request: the server gives
// them a 5-minute deadline (vs. the 30-second default elsewhere), so the
// client waits out that deadline plus a margin rather than aborting first.
const KNOWLEDGE_BULK_TIMEOUT_MS = 5 * 60_000 + 30_000

const VERBATIM_ERROR_CODES = new Set([
  'KNOWLEDGE_BUSY',
  'EMBEDDING_RATE_LIMITED',
  'EMBEDDING_PERMISSION_DENIED',
  'EMBEDDING_AUTH_FAILED',
  'EMBEDDING_PROVIDER_REJECTED',
  // The organisation's daily AI budget is spent (429); the message carries
  // the figures.
  'BUDGET_EXCEEDED',
  // The budget could not be checked (503); the server message says why.
  'BUDGET_UNAVAILABLE',
])

export default function KnowledgeBasePanel() {
  const {t} = useTranslation('settings')
  const organisations = useOrgStore(s => s.organisations)
  const activeOrgId = useOrgStore(s => s.activeOrgId)
  const currentUser = useAuthStore(s => s.user)
  const activeOrg = useMemo(() => organisations.find(o => o.id === activeOrgId), [organisations, activeOrgId])
  const canManage = useMemo(() => {
    if (!activeOrg || !currentUser) return false
    // Admin only: upload/delete/re-index are admin-gated server-side
    // (requireAdmin); showing members those controls just produced 403s.
    const membership = activeOrg.members.find(m => m.userId === currentUser.id)
    return membership?.role === 'admin'
  }, [activeOrg, currentUser])

  const [uploading, setUploading] = useState(false)
  const [uploadError, setUploadError] = useState<string | null>(null)
  const [selectedFile, setSelectedFile] = useState<File | null>(null)
  // Mirrors the backend's maxKnowledgeUploadBytes (1 MiB decoded content) —
  // the old parser.maxFileSizeMB label (50 MB default) let files pass the
  // client check and then 413 at the server.
  const MAX_UPLOAD_BYTES = 1024 * 1024
  const [reindexing, setReindexing] = useState(false)
  // Chunks re-indexed so far by a multi-request (cursor) re-index; null
  // before the first page lands.
  const [reindexProgress, setReindexProgress] = useState<number | null>(null)
  // Stops a running re-index: aborts the in-flight page, and with it the
  // loop. Pages already answered stay committed server-side.
  const reindexAbortRef = useRef<AbortController | null>(null)
  useEffect(() => () => reindexAbortRef.current?.abort(), [])
  const {confirm} = useConfirm()
  const {success: toastSuccess, error: toastError, info: toastInfo} = useToast()

  const {
    data,
    isLoading: loading,
    error: fetchError,
    refetch: loadDocs,
  } = useAsync<KnowledgeDoc[]>(() => {
    if (!activeOrgId) return Promise.resolve([])
    return request<KnowledgeDoc[]>(`/api/orgs/${activeOrgId}/knowledge`)
      .then(res => res || [])
      .catch(e => {
        logger.warn(e)
        throw e
      })
  }, [activeOrgId])
  const docs = data ?? []
  const loadError = fetchError ? t('knowledge.loadFailed') : null

  // providerVerdict maps an upload/re-index failure to the text to show, or
  // null when it is not one of the embedding-provider outcomes both share.
  const providerVerdict = (e: unknown): string | null => {
    if (!(e instanceof ApiError) || !e.code) return null
    if (VERBATIM_ERROR_CODES.has(e.code)) return e.message
    if (e.code === 'EMBEDDING_PROVIDER_UNAVAILABLE') return t('knowledge.embeddingUnavailable')
    // 504: the request hit the server's deadline before any page committed.
    if (e.code === 'KNOWLEDGE_TIMEOUT') return t('knowledge.timeout')
    return null
  }

  const handleUpload = async () => {
    if (!activeOrgId || !selectedFile) return
    setUploadError(null)
    if (selectedFile.size > MAX_UPLOAD_BYTES) {
      setUploadError(t('knowledge.fileTooLarge', {size: (selectedFile.size / 1024 / 1024).toFixed(1)}))
      return
    }
    setUploading(true)
    try {
      const content = await selectedFile.text()
      await request(`/api/orgs/${activeOrgId}/knowledge/upload`, {
        body: {
          filename: selectedFile.name,
          content,
        },
        timeoutMs: KNOWLEDGE_BULK_TIMEOUT_MS,
      })
      setSelectedFile(null)
      toastSuccess(t('knowledge.uploaded'))
      loadDocs()
    } catch (e) {
      // The most common failure is a missing/unconfigured embedding-provider
      // key (only OpenAI/Gemini/GLM/GitHub Models/Azure AI Foundry support
      // embeddings; a Claude-only setup can't index). Surface that root cause
      // with clear guidance instead of a generic "Upload failed". Classify by
      // the envelope's machine-readable code first (stable across message
      // rewording), with the message regex as a fallback for older backends.
      const verdict = providerVerdict(e)
      const embeddingMissing =
        (e instanceof ApiError && e.code === 'EMBEDDING_NOT_CONFIGURED') || /embedding provider/i.test(String(e))
      if (verdict) {
        // Before the message regex: a provider verdict's message mentions
        // the embedding provider too, and it is not a missing key.
        setUploadError(verdict)
      } else if (embeddingMissing) {
        setUploadError(t('knowledge.embeddingMissingUpload'))
      } else if (e instanceof ApiError && e.code === 'EMBEDDING_MODEL_CHANGED') {
        // 409: the uploader's embedding model is not the corpus's — mixing
        // them would make the document unfindable for everyone else.
        setUploadError(t('knowledge.modelChangedUpload'))
      } else {
        setUploadError(t('knowledge.uploadFailedDetail', {message: String(e)}))
        toastError(t('knowledge.uploadFailed'))
      }
    } finally {
      setUploading(false)
    }
  }

  const handleDelete = async (docId: string) => {
    if (!activeOrgId) return
    const filename = docs.find(d => d.id === docId)?.filename
    const ok = await confirm({
      title: t('knowledge.deleteTitle'),
      message: filename ? t('knowledge.deleteNamedMessage', {filename}) : t('knowledge.deleteMessage'),
      danger: true,
      confirmLabel: t('knowledge.deleteConfirm'),
    })
    if (!ok) return
    try {
      await request(`/api/orgs/${activeOrgId}/knowledge/${docId}`, {body: {}, method: 'DELETE'})
      toastSuccess(t('knowledge.deleted'))
      loadDocs()
    } catch (e) {
      logger.warn(e)
      toastError(t('knowledge.deleteFailed'))
    }
  }

  const handleReindex = async () => {
    if (!activeOrgId) return
    const ok = await confirm({
      title: t('knowledge.reindexConfirmTitle'),
      message: t('knowledge.reindexConfirmMessage'),
      confirmLabel: t('knowledge.reindexConfirmLabel'),
    })
    if (!ok) return
    const controller = new AbortController()
    reindexAbortRef.current = controller
    const reindex = (force: boolean, cursor?: Record<string, unknown>) =>
      request<ReindexResult>(`/api/orgs/${activeOrgId}/knowledge/reindex`, {
        body: {...(force ? {force: true} : {}), ...(cursor ? {cursor} : {})},
        timeoutMs: KNOWLEDGE_BULK_TIMEOUT_MS,
        signal: controller.signal,
      })
    setReindexing(true)
    setReindexProgress(null)
    // Totals across the pages of a resumable re-index. A document can span
    // pages, so only the chunk total is summed; the document count is shown
    // for a single-request run only.
    let chunks = 0
    let pages = 0
    try {
      let force = false
      let res: ReindexResult
      try {
        res = await reindex(false)
      } catch (e) {
        // 409: the corpus was indexed with a different embedding model than
        // the admin's. Re-indexing anyway strands every member on that model,
        // so it takes a second, explicit confirmation before resending with
        // force.
        if (!(e instanceof ApiError && e.code === 'EMBEDDING_MODEL_CHANGED')) throw e
        force = await confirm({
          title: t('knowledge.reindexModelChangedTitle'),
          message: t('knowledge.reindexModelChanged'),
          confirmLabel: t('knowledge.reindexForceLabel'),
          danger: true,
        })
        if (!force) return
        res = await reindex(true)
      }
      chunks += res?.chunks ?? 0
      pages++
      // Resumable server: each answer committed its page; keep going from
      // where it stopped until no cursor comes back.
      while (res?.nextCursor) {
        setReindexProgress(chunks)
        res = await reindex(force, res.nextCursor)
        chunks += res?.chunks ?? 0
        pages++
      }
      if (res?.truncated) {
        // Not a success: documents are still waiting on the next run.
        toastError(t('knowledge.reindexTruncated', {chunks}))
      } else if (chunks === 0) {
        toastSuccess(t('knowledge.reindexNothing'))
      } else {
        toastSuccess(
          pages === 1
            ? t('knowledge.reindexDone', {chunks, docs: res?.docs ?? 0})
            : t('knowledge.reindexDoneChunks', {count: chunks}),
        )
      }
      loadDocs()
    } catch (e) {
      if (isAbortError(e)) {
        // Stopped by the admin (or the panel closed): the pages that were
        // answered stay re-indexed; a new run starts over from the top.
        toastInfo(t('knowledge.reindexStopped', {count: chunks}))
        loadDocs()
        return
      }
      logger.warn(e)
      // A later page refused because the admin's embedding model is not the one
      // that converted the earlier pages (the model changed mid-run): the run
      // has to start over, so say that instead of a bare failure.
      const midRunModelChange = pages > 0 && e instanceof ApiError && e.code === 'EMBEDDING_MODEL_CHANGED'
      const verdict = midRunModelChange ? t('knowledge.reindexModelChangedMidRun') : providerVerdict(e)
      const embeddingMissing =
        (e instanceof ApiError && e.code === 'EMBEDDING_NOT_CONFIGURED') || /embedding provider/i.test(String(e))
      toastError(verdict ?? (embeddingMissing ? t('knowledge.embeddingMissingReindex') : t('knowledge.reindexFailed')))
      // Earlier pages of a resumable run were committed before the failure.
      if (pages > 0) loadDocs()
    } finally {
      if (reindexAbortRef.current === controller) reindexAbortRef.current = null
      setReindexing(false)
      setReindexProgress(null)
    }
  }

  if (!activeOrgId) {
    return (
      <div className="flex flex-col items-center justify-center h-64 text-text-tertiary text-center px-4">
        <p>{t('knowledge.selectOrgFirst')}</p>
      </div>
    )
  }

  return (
    <div>
      <p className="text-sm text-text-secondary">{t('knowledge.intro')}</p>
      <p className="text-xs text-text-tertiary mt-1 mb-6">
        {t('knowledge.orgLabel')} <span className="font-medium text-text-primary">{activeOrg?.name}</span>
      </p>

      <div className="bg-brand-500/5 border border-brand-500/20 rounded-lg p-3 mb-6 text-xs text-text-secondary">
        <strong className="text-text-primary">{t('knowledge.embeddingNoticePrefix')}</strong>{' '}
        {t('knowledge.embeddingNoticeSuffix')}
      </div>

      {canManage && (
        <div className="bg-surface-2 rounded-lg border border-border-default p-4 mb-8">
          <h3 className="text-sm font-medium text-text-primary mb-3">{t('knowledge.uploadTitle')}</h3>
          <div className="flex items-center gap-3">
            <label className="flex-1 flex items-center gap-2 px-3 py-2 bg-surface-3 border border-border-default rounded-md cursor-pointer hover:bg-surface-4 transition-colors overflow-hidden">
              <Upload className="w-4 h-4 text-text-secondary shrink-0" />
              <span className="text-sm text-text-primary truncate">
                {selectedFile ? selectedFile.name : t('knowledge.selectFile')}
              </span>
              <input
                type="file"
                className="hidden"
                accept=".txt,.md"
                onChange={e => {
                  setSelectedFile(e.target.files?.[0] || null)
                  setUploadError(null)
                }}
              />
            </label>
            <Button variant="primary" disabled={!selectedFile || uploading} onClick={handleUpload}>
              {uploading ? <Loader2 className="w-4 h-4 animate-spin" /> : t('knowledge.indexDocument')}
            </Button>
          </div>
          <p className="text-2xs text-text-tertiary mt-2">{t('knowledge.uploadHint')}</p>
          {uploadError && (
            <p role="alert" className="text-xs text-red-400 mt-2">
              {uploadError}
            </p>
          )}
        </div>
      )}

      <div className="space-y-3">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-medium text-text-primary">{t('knowledge.indexedTitle')}</h3>
          {canManage && docs.length > 0 && (
            <div className="flex items-center gap-2">
              {reindexing && (
                <button
                  onClick={() => reindexAbortRef.current?.abort()}
                  className="flex items-center gap-1.5 px-2.5 py-1 text-xs text-text-secondary hover:text-red-400 border border-border-default hover:border-red-400/50 rounded-md transition-colors"
                  title={t('knowledge.reindexStopTitle')}
                >
                  <Square className="w-3 h-3" />
                  {t('knowledge.reindexStop')}
                </button>
              )}
              <button
                onClick={handleReindex}
                disabled={reindexing}
                className="flex items-center gap-1.5 px-2.5 py-1 text-xs text-text-secondary hover:text-brand-400 border border-border-default hover:border-brand-500/50 rounded-md transition-colors disabled:opacity-50"
                title={t('knowledge.reindexTitle')}
              >
                {reindexing ? <Loader2 className="w-3.5 h-3.5 animate-spin" /> : <RefreshCw className="w-3.5 h-3.5" />}
                {reindexing ? t('knowledge.reindexing') : t('knowledge.reindex')}
              </button>
            </div>
          )}
        </div>
        {reindexProgress !== null && (
          <p role="status" className="text-xs text-text-secondary">
            {t('knowledge.reindexProgress', {count: reindexProgress})}
          </p>
        )}
        {loading ? (
          <div className="flex justify-center p-8">
            <Loader2 className="w-6 h-6 animate-spin text-brand-500" />
          </div>
        ) : loadError ? (
          <ErrorState message={loadError} onRetry={loadDocs} />
        ) : docs.length === 0 ? (
          <div className="text-center py-12 bg-surface-1 rounded-lg border border-dashed border-border-default text-text-tertiary">
            {t('knowledge.empty')}
          </div>
        ) : (
          <div className="grid gap-2">
            {docs.map(doc => (
              <div
                key={doc.id}
                className="flex items-center justify-between p-3 bg-surface-2 border border-border-default rounded-lg group hover:border-brand-500/50 transition-colors"
              >
                <div className="flex items-center gap-3 min-w-0 flex-1">
                  <div className="p-2 bg-brand-500/10 rounded shrink-0">
                    <FileText className="w-5 h-5 text-brand-500" />
                  </div>
                  <div className="min-w-0">
                    <div title={doc.filename} className="text-sm font-medium text-text-primary truncate">
                      {doc.filename}
                    </div>
                    <div className="text-xs text-text-tertiary" title={absoluteTime(doc.createdAt)}>
                      {t('knowledge.indexedAgo', {when: relativeTime(doc.createdAt)})}
                      {doc.chunkCount != null && doc.chunkCount > 0 && (
                        <>
                          {' · '}
                          {doc.vectorIndexed != null
                            ? t('knowledge.chunksSearchable', {count: doc.chunkCount, searchable: doc.vectorIndexed})
                            : t('knowledge.chunks', {count: doc.chunkCount})}
                        </>
                      )}
                    </div>
                    {(doc.chunkCount ?? 0) > 0 && (doc.vectorIndexed ?? 0) === 0 && (
                      <div className="text-xs text-amber-400">{t('knowledge.dimensionMismatch')}</div>
                    )}
                  </div>
                </div>
                {canManage && (
                  <button
                    onClick={() => handleDelete(doc.id)}
                    aria-label={t('knowledge.deleteAria', {filename: doc.filename})}
                    title={t('knowledge.deleteAria', {filename: doc.filename})}
                    className="p-2 text-text-tertiary hover:text-red-400 opacity-0 group-hover:opacity-100 focus-visible:opacity-100 transition-opacity"
                  >
                    <Trash2 className="w-4 h-4" />
                  </button>
                )}
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}
