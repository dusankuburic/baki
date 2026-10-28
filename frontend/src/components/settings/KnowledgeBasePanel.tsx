import {getErrorCode, getErrorMessage} from '@/lib/getErrorMessage'
import {useState, useMemo, useRef, useEffect} from 'react'
import {useOrgStore} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {azureScopeKey, useAzureFoundryStore} from '@/stores/azureFoundryStore'
import {request, ApiError} from '@/api/client'
import {Trash2, FileText, Loader2, Upload, RefreshCw, Square, Scissors} from 'lucide-react'
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
  embeddingStranded?: number
  // Present on a document with chunks outside the vector index when the server measured why: the deployment's
  // width (PAD_EMBEDDING_DIM) and the one on the side source names — what the embedding model produces (set
  // PAD_EMBEDDING_DIM to it and restart) or what the stored chunks hold (re-index).
  embeddingWidthMismatch?: {configured: number; observed: number; source: 'embedder' | 'stored'}
  // True on a deployment without a vector index: no chunk is ever in one there, which says nothing about
  // searchability (searches run in-process), so nothing is "not searchable" for being outside it.
  noVectorIndex?: boolean
  // The chunker the document was cut with; hasSource: its text is kept, so a re-chunk can cut it again.
  chunkerVersion?: number
  hasSource?: boolean
  // "pending": an upload whose pendingChunks are still to embed; none of it is searched until it is "ready".
  state?: 'pending' | 'ready'
  pendingChunks?: number
}

interface ReindexResult {
  chunks: number
  docs: number
  truncated?: boolean
  nextCursor?: Record<string, unknown> | null
  // A rechunk run: documents cut again this request, and those it could not (no kept text, too many passages).
  rechunked?: number
  skippedNoSource?: number
  skippedTooLarge?: number
  // Left alone because an unfinished upload of the same filename exists (a re-chunk would supersede it).
  skippedPending?: number
}

interface UploadCursor {
  docId: string
  id: string
}

// One upload request: the document is stored first, then embedded page by page within the server's budget; a
// pending answer is continued by its id ({docId, cursor?}) until no passage is left.
interface UploadResult {
  id: string
  filename: string
  state: 'pending' | 'ready'
  chunkerVersion: number
  chunks: number
  embedded: number
  pendingChunks: number
  resumed?: boolean
  truncated?: boolean
  nextCursor?: UploadCursor | null
}

// An upload to continue: the stored document and, when known, where the last request stopped (a hint only) and how
// far it got.
interface UploadResume {
  docId: string
  cursor?: UploadCursor
  progress?: UploadProgress
}

// A type, not an interface: it is passed as i18next interpolation values, which need an index signature.
type UploadProgress = {
  embedded: number
  chunks: number
}

// The reason a run is aborted when the panel unmounts or switches organisation: the run then ends without touching
// the panel's state, which belongs to another organisation or to nothing.
const PANEL_GONE = new DOMException('The knowledge panel closed or switched organisation', 'AbortError')

/** The chunker version new uploads are cut with; a document below it can be re-chunked when its text is kept. */
const CURRENT_CHUNKER_VERSION = 2

function isUploadCursor(v: unknown): v is UploadCursor {
  return !!v && typeof v === 'object' && typeof (v as UploadCursor).docId === 'string'
}

function isAbortError(e: unknown): boolean {
  return (e instanceof DOMException || e instanceof Error) && e.name === 'AbortError'
}

function isReindexCursor(v: unknown): v is Record<string, unknown> {
  return !!v && typeof v === 'object'
}

const KNOWLEDGE_BULK_TIMEOUT_MS = 5 * 60_000 + 30_000

// One upload or re-chunk request: the server stops at its request budget (90 s) and answers what is left, so a
// request is bounded well under the ingress cut (240 s on Azure Container Apps); the panel loops for the rest.
const KNOWLEDGE_UPLOAD_REQUEST_TIMEOUT_MS = 3 * 60_000

const VERBATIM_ERROR_CODES = new Set([
  'KNOWLEDGE_BUSY',
  'EMBEDDING_WIDTH_OFF',
  'EMBEDDING_RATE_LIMITED',
  'EMBEDDING_PERMISSION_DENIED',
  'EMBEDDING_AUTH_FAILED',
  'EMBEDDING_PROVIDER_REJECTED',
  'KNOWLEDGE_DOCUMENT_TOO_LARGE',
  'EMBEDDING_ORG_NOT_CONFIGURED',
  'KNOWLEDGE_FILENAME_INVALID',
])

export default function KnowledgeBasePanel() {
  const {t} = useTranslation('settings')
  const organisations = useOrgStore(s => s.organisations)
  const activeOrgId = useOrgStore(s => s.activeOrgId)
  const currentUser = useAuthStore(s => s.user)
  const activeOrg = useMemo(() => organisations.find(o => o.id === activeOrgId), [organisations, activeOrgId])
  const canManage = useMemo(() => {
    if (!activeOrg || !currentUser) return false
    const membership = activeOrg.members.find(m => m.userId === currentUser.id)
    return membership?.role === 'admin'
  }, [activeOrg, currentUser])

  const [uploading, setUploading] = useState(false)
  const [uploadError, setUploadError] = useState<string | null>(null)
  const [selectedFile, setSelectedFile] = useState<File | null>(null)
  const MAX_UPLOAD_BYTES = 1024 * 1024
  const [reindexing, setReindexing] = useState(false)
  const [reindexProgress, setReindexProgress] = useState<number | null>(null)
  const reindexAbortRef = useRef<AbortController | null>(null)
  const [resumable, setResumable] = useState<{cursor: Record<string, unknown>; force: boolean} | null>(null)
  const [uploadProgress, setUploadProgress] = useState<UploadProgress | null>(null)
  const [uploadResumable, setUploadResumable] = useState<UploadResume | null>(null)
  const uploadAbortRef = useRef<AbortController | null>(null)
  const [rechunking, setRechunking] = useState(false)
  // A run belongs to the organisation it started in: switching (or leaving) stops it quietly, and what it left to
  // resume or report is forgotten, so nothing of one organisation is ever posted to another.
  useEffect(
    () => () => {
      reindexAbortRef.current?.abort(PANEL_GONE)
      uploadAbortRef.current?.abort(PANEL_GONE)
    },
    [activeOrgId],
  )
  const [orgSeen, setOrgSeen] = useState(activeOrgId)
  if (orgSeen !== activeOrgId) {
    setOrgSeen(activeOrgId)
    setUploadResumable(null)
    setUploadError(null)
    setResumable(null)
    setSelectedFile(null)
  }
  const {confirm} = useConfirm()
  const {success: toastSuccess, error: toastError, info: toastInfo} = useToast()
  const modelChanged = useAzureFoundryStore(
    s =>
      !!activeOrgId &&
      (!!s.knowledgeReindex[azureScopeKey({kind: 'org', orgId: activeOrgId})] ||
        !!s.knowledgeReindex[azureScopeKey({kind: 'user'})]),
  )
  const clearModelChanged = () => {
    if (!activeOrgId) return
    useAzureFoundryStore.getState().clearKnowledgeReindex([{kind: 'org', orgId: activeOrgId}, {kind: 'user'}])
  }

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

  const providerVerdict = (e: unknown): string | null => {
    if (!(e instanceof ApiError) || !e.code) return null
    if (VERBATIM_ERROR_CODES.has(e.code)) return e.message
    if (e.code === 'EMBEDDING_PROVIDER_UNAVAILABLE') return t('knowledge.embeddingUnavailable')
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
    const file = selectedFile
    await runUpload(async () => ({filename: file.name, content: await file.text()}))
  }

  // Uploads a document, or continues a stored one, until none of its passages is pending (R-knowledge-2). Each
  // request embeds what fits in the server's budget; the loop follows the document by id, stoppable, and a stop or a
  // failure after the document was stored leaves a Resume that continues it, so no passage is paid for twice.
  const runUpload = async (first: () => Promise<Record<string, unknown>>, resume?: UploadResume) => {
    if (!activeOrgId) return
    setUploadError(null)
    setUploadResumable(null)
    const controller = new AbortController()
    uploadAbortRef.current = controller
    const post = async (body: Record<string, unknown>) => {
      const res = await request<UploadResult>(`/api/orgs/${activeOrgId}/knowledge/upload`, {
        body,
        timeoutMs: KNOWLEDGE_UPLOAD_REQUEST_TIMEOUT_MS,
        signal: controller.signal,
      })
      // An answer that arrives after a stop (or after the panel went) ends the run all the same.
      if (controller.signal.aborted) throw controller.signal.reason
      return res
    }
    const continuation = (at: UploadResume) => ({docId: at.docId, ...(at.cursor ? {cursor: at.cursor} : {})})
    let at: UploadResume | undefined = resume
    let progress: UploadProgress | null = resume?.progress ?? null
    setUploading(true)
    setUploadProgress(progress)
    try {
      let res = await post(resume ? continuation(resume) : await first())
      for (;;) {
        at = {docId: res.id, ...(isUploadCursor(res.nextCursor) ? {cursor: res.nextCursor} : {})}
        progress = {embedded: res.chunks - res.pendingChunks, chunks: res.chunks}
        if (res.pendingChunks <= 0) break
        setUploadProgress(progress)
        const before = res.pendingChunks
        res = await post(continuation(at))
        if (res.pendingChunks > 0 && res.pendingChunks >= before && !res.embedded) {
          // A continuation that embedded nothing would only repeat itself: leave it to a Resume.
          at = {docId: res.id, ...(isUploadCursor(res.nextCursor) ? {cursor: res.nextCursor} : {})}
          setUploadError(t('knowledge.uploadStalled'))
          setUploadResumable({...at, ...(progress ? {progress} : {})})
          loadDocs()
          return
        }
      }
      // The file is done once its document is: a resume of another document leaves a file picked meanwhile.
      const doneName = res.filename
      setSelectedFile(f => (!resume || f?.name === doneName ? null : f))
      toastSuccess(t('knowledge.uploaded'))
      loadDocs()
    } catch (e) {
      if (controller.signal.reason === PANEL_GONE) return
      // The server may have stored the document whatever failed: the list shows it, pending, with its Resume.
      loadDocs()
      if (isAbortError(e)) {
        if (at) setUploadResumable({...at, ...(progress ? {progress} : {})})
        toastInfo(progress ? t('knowledge.uploadStopped', progress) : t('knowledge.uploadStoppedUnknown'))
        return
      }
      logger.warn(e)
      const code = getErrorCode(e)
      if (code === 'KNOWLEDGE_DOCUMENT_NOT_FOUND') {
        // The upload to continue was replaced by a newer one of its name, or deleted: nothing is left to resume.
        setUploadError(t('knowledge.uploadDocumentGone'))
        return
      }
      // A request that fails after the document was stored names it, and where to continue.
      const details = e instanceof ApiError ? e.details : undefined
      const storedId = typeof details?.id === 'string' && details.id ? details.id : at?.docId
      if (storedId) {
        const cursor = isUploadCursor(details?.nextCursor) ? details.nextCursor : at?.cursor
        setUploadResumable({docId: storedId, ...(cursor ? {cursor} : {}), ...(progress ? {progress} : {})})
      }
      const verdict = providerVerdict(e)
      const embeddingMissing = code === 'EMBEDDING_NOT_CONFIGURED' || /embedding provider/i.test(String(e))
      if (verdict) {
        setUploadError(verdict)
      } else if (embeddingMissing) {
        setUploadError(t('knowledge.embeddingMissingUpload'))
      } else if (code === 'EMBEDDING_MODEL_CHANGED') {
        setUploadError(t('knowledge.modelChangedUpload'))
      } else {
        setUploadError(t('knowledge.uploadFailedDetail', {message: getErrorMessage(e)}))
        toastError(t('knowledge.uploadFailed'))
      }
    } finally {
      if (uploadAbortRef.current === controller) uploadAbortRef.current = null
      if (controller.signal.reason !== PANEL_GONE) {
        setUploading(false)
        setUploadProgress(null)
      }
    }
  }

  const resumeUpload = (at: UploadResume) => void runUpload(() => Promise.resolve({}), at)

  // A pending row's Resume starts from what the list knows of it, so a stop before the first answer still says how
  // far it got.
  const resumePendingDoc = (doc: KnowledgeDoc) =>
    resumeUpload({
      docId: doc.id,
      ...(doc.chunkCount
        ? {progress: {embedded: Math.max(0, doc.chunkCount - (doc.pendingChunks ?? 0)), chunks: doc.chunkCount}}
        : {}),
    })

  // Re-chunks the documents an older chunker cut, from their kept text (R-knowledge-9). The server picks the
  // documents below the current version itself, so the run repeats {rechunk: true} while it reports more to do; a
  // stopped run is simply started again. Each re-chunked document becomes a new version with a new id.
  const handleRechunk = async () => {
    if (!activeOrgId) return
    const ok = await confirm({
      title: t('knowledge.rechunkConfirmTitle'),
      message: t('knowledge.rechunkConfirmMessage'),
      confirmLabel: t('knowledge.rechunk'),
    })
    if (!ok) return
    const controller = new AbortController()
    reindexAbortRef.current = controller
    setReindexing(true)
    setRechunking(true)
    setReindexProgress(null)
    let rechunked = 0
    let skippedNoSource = 0
    let skippedTooLarge = 0
    let skippedPending = 0
    let incomplete = false
    try {
      for (;;) {
        const res = await request<ReindexResult>(`/api/orgs/${activeOrgId}/knowledge/reindex`, {
          body: {rechunk: true},
          timeoutMs: KNOWLEDGE_UPLOAD_REQUEST_TIMEOUT_MS,
          signal: controller.signal,
        })
        if (controller.signal.aborted) throw controller.signal.reason
        rechunked += res?.rechunked ?? 0
        // Every request walks the documents still below the current version: one it skips is counted again.
        skippedNoSource = Math.max(skippedNoSource, res?.skippedNoSource ?? 0)
        skippedTooLarge = Math.max(skippedTooLarge, res?.skippedTooLarge ?? 0)
        skippedPending = Math.max(skippedPending, res?.skippedPending ?? 0)
        if (!res?.nextCursor) break
        if (!res.rechunked && !res.chunks) {
          // More is left but this request got nowhere: repeating it would loop. The run can simply be started again.
          incomplete = true
          break
        }
        setReindexProgress(rechunked)
      }
      if (incomplete) toastInfo(t('knowledge.rechunkIncomplete', {count: rechunked}))
      else toastSuccess(rechunked > 0 ? t('knowledge.rechunkDone', {count: rechunked}) : t('knowledge.rechunkNothing'))
      if (skippedNoSource > 0) toastInfo(t('knowledge.rechunkSkippedNoSource', {count: skippedNoSource}))
      if (skippedTooLarge > 0) toastInfo(t('knowledge.rechunkSkippedTooLarge', {count: skippedTooLarge}))
      if (skippedPending > 0) toastInfo(t('knowledge.rechunkSkippedPending', {count: skippedPending}))
    } catch (e) {
      if (controller.signal.reason === PANEL_GONE) return
      if (isAbortError(e)) {
        toastInfo(t('knowledge.rechunkStopped', {count: rechunked}))
      } else {
        logger.warn(e)
        toastError(rechunkVerdict(e))
      }
    } finally {
      if (controller.signal.reason !== PANEL_GONE) loadDocs()
      if (reindexAbortRef.current === controller) reindexAbortRef.current = null
      setReindexing(false)
      setRechunking(false)
      setReindexProgress(null)
    }
  }

  const rechunkVerdict = (e: unknown): string => {
    const code = getErrorCode(e)
    if (code === 'EMBEDDING_MODEL_CHANGED') return t('knowledge.rechunkModelChanged')
    if (code === 'EMBEDDING_NOT_CONFIGURED') return t('knowledge.embeddingMissingReindex')
    return providerVerdict(e) ?? t('knowledge.rechunkFailed')
  }

  // Not while an upload is pending or running: a re-chunk supersedes documents by name, a paused upload with them.
  const canRechunk =
    !uploading &&
    !uploadResumable &&
    !docs.some(d => d.state === 'pending') &&
    docs.some(d => !!d.hasSource && (d.chunkerVersion ?? 1) < CURRENT_CHUNKER_VERSION)

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

  const handleReindex = async (resume?: {cursor: Record<string, unknown>; force: boolean}) => {
    if (!activeOrgId) return
    if (!resume) {
      const ok = await confirm({
        title: t('knowledge.reindexConfirmTitle'),
        message: t('knowledge.reindexConfirmMessage'),
        confirmLabel: t('knowledge.reindexConfirmLabel'),
      })
      if (!ok) return
    }
    setResumable(null)
    const controller = new AbortController()
    reindexAbortRef.current = controller
    const reindex = async (force: boolean, cursor?: Record<string, unknown>) => {
      const res = await request<ReindexResult>(`/api/orgs/${activeOrgId}/knowledge/reindex`, {
        body: {...(force ? {force: true} : {}), ...(cursor ? {cursor} : {})},
        timeoutMs: KNOWLEDGE_BULK_TIMEOUT_MS,
        signal: controller.signal,
      })
      if (controller.signal.aborted) throw controller.signal.reason
      return res
    }
    setReindexing(true)
    setReindexProgress(null)
    let chunks = 0
    let pages = 0
    let force = resume?.force ?? false
    let lastCursor: Record<string, unknown> | undefined = resume?.cursor
    try {
      let res: ReindexResult
      if (lastCursor) {
        res = await reindex(force, lastCursor)
      } else {
        try {
          res = await reindex(false)
        } catch (e) {
          const code = getErrorCode(e)
          if (code === 'EMBEDDING_WIDTH_OFF') {
            // The server names both widths and the pin remedy; forcing is the admin's word to re-embed at the
            // provider's width anyway, the step before a restart at that width.
            force = await confirm({
              title: t('knowledge.reindexWidthOffTitle'),
              message: t('knowledge.reindexWidthOff', {reason: e instanceof ApiError ? e.message : String(e)}),
              confirmLabel: t('knowledge.reindexWidthOffForceLabel'),
              danger: true,
            })
          } else if (code === 'EMBEDDING_MODEL_CHANGED') {
            force = await confirm({
              title: t('knowledge.reindexModelChangedTitle'),
              message: t('knowledge.reindexModelChanged'),
              confirmLabel: t('knowledge.reindexForceLabel'),
              danger: true,
            })
          } else {
            throw e
          }
          if (!force) return
          res = await reindex(true)
        }
      }
      chunks += res?.chunks ?? 0
      pages++
      lastCursor = res?.nextCursor ?? undefined
      while (lastCursor) {
        setReindexProgress(chunks)
        res = await reindex(force, lastCursor)
        chunks += res?.chunks ?? 0
        pages++
        lastCursor = res?.nextCursor ?? undefined
      }
      if (res?.truncated) {
        toastError(t('knowledge.reindexTruncated', {chunks}))
      } else if (chunks === 0) {
        clearModelChanged()
        toastSuccess(t('knowledge.reindexNothing'))
      } else {
        clearModelChanged()
        toastSuccess(
          pages === 1
            ? t('knowledge.reindexDone', {chunks, docs: res?.docs ?? 0})
            : t('knowledge.reindexDoneChunks', {count: chunks}),
        )
      }
      loadDocs()
    } catch (e) {
      if (controller.signal.reason === PANEL_GONE) return
      if (isAbortError(e)) {
        toastInfo(
          lastCursor
            ? t('knowledge.reindexStopped', {count: chunks})
            : t('knowledge.reindexStoppedRestart', {count: chunks}),
        )
        loadDocs()
        if (lastCursor) setResumable({cursor: lastCursor, force})
        return
      }
      logger.warn(e)
      const midRunModelChange = (pages > 0 || !!resume) && getErrorCode(e) === 'EMBEDDING_MODEL_CHANGED'
      const verdict = midRunModelChange ? t('knowledge.reindexModelChangedMidRun') : providerVerdict(e)
      const embeddingMissing = getErrorCode(e) === 'EMBEDDING_NOT_CONFIGURED' || /embedding provider/i.test(String(e))
      toastError(verdict ?? (embeddingMissing ? t('knowledge.embeddingMissingReindex') : t('knowledge.reindexFailed')))
      if (pages > 0) loadDocs()
      if (!midRunModelChange) {
        const failedAt =
          e instanceof ApiError && isReindexCursor(e.details?.nextCursor) ? e.details.nextCursor : lastCursor
        if (failedAt) setResumable({cursor: failedAt, force})
      }
    } finally {
      if (reindexAbortRef.current === controller) reindexAbortRef.current = null
      if (controller.signal.reason !== PANEL_GONE) {
        setReindexing(false)
        setReindexProgress(null)
      }
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
        <p className="mt-2">{t('knowledge.memberEmbeddingHint')}</p>
      </div>

      {modelChanged && docs.length > 0 && (
        <div className="mb-6 flex flex-wrap items-center gap-2 rounded-lg border border-semantic-warning/30 bg-semantic-warning/10 px-3 py-2 text-xs text-text-secondary">
          <p className="min-w-0 flex-1">
            {canManage ? t('knowledge.reindexAfterModelChange') : t('knowledge.reindexAfterModelChangeMember')}
          </p>
          {canManage && (
            <Button size="sm" variant="secondary" onClick={() => void handleReindex()} disabled={reindexing}>
              {t('knowledge.reindex')}
            </Button>
          )}
        </div>
      )}

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
            <Button
              variant="primary"
              disabled={!selectedFile || uploading}
              onClick={handleUpload}
              aria-label={uploading ? t('knowledge.indexing') : undefined}
            >
              {uploading ? <Loader2 className="w-4 h-4 animate-spin" /> : t('knowledge.indexDocument')}
            </Button>
            {uploading && (
              <Button variant="secondary" onClick={() => uploadAbortRef.current?.abort()}>
                <Square className="w-3 h-3" />
                {t('knowledge.uploadStop')}
              </Button>
            )}
            {uploadResumable && !uploading && (
              <Button variant="secondary" onClick={() => resumeUpload(uploadResumable)}>
                <RefreshCw className="w-3.5 h-3.5" />
                {t('knowledge.uploadResume')}
              </Button>
            )}
          </div>
          <p
            role="status"
            aria-label={t('knowledge.uploadProgressLabel')}
            className="text-xs text-text-secondary mt-2 empty:hidden"
          >
            {uploadProgress ? t('knowledge.uploadProgress', uploadProgress) : null}
          </p>
          <p className="text-2xs text-text-tertiary mt-2">{t('knowledge.uploadHint')}</p>
          <p className="text-2xs text-text-tertiary mt-1">{t('knowledge.uploadScrubHint')}</p>
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
              {resumable && !reindexing && (
                <button
                  onClick={() => void handleReindex(resumable)}
                  className="flex items-center gap-1.5 px-2.5 py-1 text-xs text-text-secondary hover:text-brand-400 border border-brand-500/40 hover:border-brand-500/70 rounded-md transition-colors"
                  title={t('knowledge.reindexResumeTitle')}
                >
                  <RefreshCw className="w-3.5 h-3.5" />
                  {t('knowledge.reindexResume')}
                </button>
              )}
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
              {canRechunk && !reindexing && (
                <button
                  onClick={() => void handleRechunk()}
                  className="flex items-center gap-1.5 px-2.5 py-1 text-xs text-text-secondary hover:text-brand-400 border border-border-default hover:border-brand-500/50 rounded-md transition-colors"
                  title={t('knowledge.rechunkTitle')}
                >
                  <Scissors className="w-3.5 h-3.5" />
                  {t('knowledge.rechunk')}
                </button>
              )}
              <button
                onClick={() => void handleReindex()}
                disabled={reindexing}
                className="flex items-center gap-1.5 px-2.5 py-1 text-xs text-text-secondary hover:text-brand-400 border border-border-default hover:border-brand-500/50 rounded-md transition-colors disabled:opacity-50"
                title={t('knowledge.reindexTitle')}
              >
                {reindexing ? <Loader2 className="w-3.5 h-3.5 animate-spin" /> : <RefreshCw className="w-3.5 h-3.5" />}
                {rechunking
                  ? t('knowledge.rechunking')
                  : reindexing
                    ? t('knowledge.reindexing')
                    : t('knowledge.reindex')}
              </button>
            </div>
          )}
        </div>
        <p
          role="status"
          aria-label={t('knowledge.reindexProgressLabel')}
          className="text-xs text-text-secondary empty:hidden"
        >
          {reindexProgress === null
            ? null
            : rechunking
              ? t('knowledge.rechunkProgress', {count: reindexProgress})
              : t('knowledge.reindexProgress', {count: reindexProgress})}
        </p>
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
                      {doc.state !== 'pending' && doc.chunkCount != null && doc.chunkCount > 0 && (
                        <>
                          {' · '}
                          {doc.vectorIndexed != null
                            ? t('knowledge.chunksSearchable', {count: doc.chunkCount, searchable: doc.vectorIndexed})
                            : t('knowledge.chunks', {count: doc.chunkCount})}
                        </>
                      )}
                    </div>
                    {doc.state === 'pending' ? (
                      <div className="text-xs text-amber-400">
                        {t('knowledge.pending', {
                          embedded: Math.max(0, (doc.chunkCount ?? 0) - (doc.pendingChunks ?? 0)),
                          chunks: doc.chunkCount ?? 0,
                        })}
                      </div>
                    ) : doc.embeddingWidthMismatch ? (
                      <div className="text-xs text-amber-400">
                        {t(
                          doc.embeddingWidthMismatch.source === 'stored'
                            ? 'knowledge.dimensionMismatchStored'
                            : 'knowledge.dimensionMismatchEmbedder',
                          doc.embeddingWidthMismatch,
                        )}
                      </div>
                    ) : (
                      !doc.noVectorIndex &&
                      (doc.chunkCount ?? 0) > 0 &&
                      (doc.vectorIndexed ?? 0) === 0 && (
                        <div className="text-xs text-amber-400">{t('knowledge.dimensionMismatch')}</div>
                      )
                    )}
                    {doc.state !== 'pending' && (doc.embeddingStranded ?? 0) > 0 && (
                      <div className="text-xs text-amber-400">
                        {t('knowledge.identityMismatch', {count: doc.embeddingStranded})}
                      </div>
                    )}
                    {canManage &&
                      doc.state !== 'pending' &&
                      !doc.hasSource &&
                      (doc.chunkerVersion ?? 1) < CURRENT_CHUNKER_VERSION && (
                        <div className="text-xs text-text-tertiary">{t('knowledge.noSourceHint')}</div>
                      )}
                  </div>
                </div>
                {canManage && doc.state === 'pending' && (
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={uploading}
                    onClick={() => resumePendingDoc(doc)}
                    aria-label={t('knowledge.resumePendingAria', {filename: doc.filename})}
                  >
                    {t('knowledge.resumePending')}
                  </Button>
                )}
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
