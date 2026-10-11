import {getErrorCode, getErrorMessage} from '@/lib/getErrorMessage'
import {useState, useMemo, useRef, useEffect} from 'react'
import {useOrgStore} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {azureScopeKey, useAzureFoundryStore} from '@/stores/azureFoundryStore'
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
  embeddingStranded?: number
}

interface ReindexResult {
  chunks: number
  docs: number
  truncated?: boolean
  nextCursor?: Record<string, unknown> | null
}

function isAbortError(e: unknown): boolean {
  return (e instanceof DOMException || e instanceof Error) && e.name === 'AbortError'
}

function isReindexCursor(v: unknown): v is Record<string, unknown> {
  return !!v && typeof v === 'object'
}

const KNOWLEDGE_BULK_TIMEOUT_MS = 5 * 60_000 + 30_000

const VERBATIM_ERROR_CODES = new Set([
  'KNOWLEDGE_BUSY',
  'EMBEDDING_RATE_LIMITED',
  'EMBEDDING_PERMISSION_DENIED',
  'EMBEDDING_AUTH_FAILED',
  'EMBEDDING_PROVIDER_REJECTED',
  'BUDGET_EXCEEDED',
  'KNOWLEDGE_DOCUMENT_TOO_LARGE',
  'EMBEDDING_ORG_NOT_CONFIGURED',
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
  useEffect(() => () => reindexAbortRef.current?.abort(), [])
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
      const verdict = providerVerdict(e)
      const embeddingMissing = getErrorCode(e) === 'EMBEDDING_NOT_CONFIGURED' || /embedding provider/i.test(String(e))
      if (verdict) {
        setUploadError(verdict)
      } else if (embeddingMissing) {
        setUploadError(t('knowledge.embeddingMissingUpload'))
      } else if (e instanceof ApiError && e.code === 'EMBEDDING_MODEL_CHANGED') {
        setUploadError(t('knowledge.modelChangedUpload'))
      } else {
        setUploadError(t('knowledge.uploadFailedDetail', {message: getErrorMessage(e)}))
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
    const reindex = (force: boolean, cursor?: Record<string, unknown>) =>
      request<ReindexResult>(`/api/orgs/${activeOrgId}/knowledge/reindex`, {
        body: {...(force ? {force: true} : {}), ...(cursor ? {cursor} : {})},
        timeoutMs: KNOWLEDGE_BULK_TIMEOUT_MS,
        signal: controller.signal,
      })
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
          if (getErrorCode(e) !== 'EMBEDDING_MODEL_CHANGED') throw e
          force = await confirm({
            title: t('knowledge.reindexModelChangedTitle'),
            message: t('knowledge.reindexModelChanged'),
            confirmLabel: t('knowledge.reindexForceLabel'),
            danger: true,
          })
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
              <button
                onClick={() => void handleReindex()}
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
                    {(doc.embeddingStranded ?? 0) > 0 && (
                      <div className="text-xs text-amber-400">
                        {t('knowledge.identityMismatch', {count: doc.embeddingStranded})}
                      </div>
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
