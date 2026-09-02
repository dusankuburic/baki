import {useState, useMemo} from 'react'
import {useOrgStore} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {request, ApiError} from '@/api/client'
import {Trash2, FileText, Loader2, Upload, RefreshCw} from 'lucide-react'
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
  const {confirm} = useConfirm()
  const {success: toastSuccess, error: toastError} = useToast()

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
  const loadError = fetchError ? 'Failed to load documents' : null

  const handleUpload = async () => {
    if (!activeOrgId || !selectedFile) return
    setUploadError(null)
    if (selectedFile.size > MAX_UPLOAD_BYTES) {
      setUploadError(`File is ${(selectedFile.size / 1024 / 1024).toFixed(1)}MB — limit is 1MB`)
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
      })
      setSelectedFile(null)
      toastSuccess('Document added')
      loadDocs()
    } catch (e) {
      // The most common failure is a missing/unconfigured embedding-provider
      // key (only OpenAI/Gemini/GLM/GitHub Models support embeddings; a
      // Claude-only setup can't index). Surface that root cause with clear
      // guidance instead of a generic "Upload failed". Classify by the
      // envelope's machine-readable code first (stable across message
      // rewording), with the message regex as a fallback for older backends.
      const embeddingMissing =
        (e instanceof ApiError && e.code === 'EMBEDDING_NOT_CONFIGURED') || /embedding provider/i.test(String(e))
      if (embeddingMissing) {
        setUploadError(
          'Indexing requires an embedding-capable provider key (OpenAI, Gemini, GLM, or GitHub Models). Configure one under AI Behavior → Embedding Assistant.',
        )
      } else {
        setUploadError('Upload failed: ' + String(e))
        toastError('Upload failed')
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
      confirmLabel: 'Delete',
    })
    if (!ok) return
    try {
      await request(`/api/orgs/${activeOrgId}/knowledge/${docId}`, {body: {}, method: 'DELETE'})
      toastSuccess('Document deleted')
      loadDocs()
    } catch (e) {
      logger.warn(e)
      toastError('Failed to delete document')
    }
  }

  const handleReindex = async () => {
    if (!activeOrgId) return
    const ok = await confirm({
      title: 'Re-index knowledge base?',
      message: 'Every document is re-embedded with the currently configured embedding provider. This consumes embedding API usage for all indexed content.',
      confirmLabel: 'Re-index',
    })
    if (!ok) return
    setReindexing(true)
    try {
      const res = await request<{chunks: number; docs: number}>(`/api/orgs/${activeOrgId}/knowledge/reindex`, {
        body: {},
      })
      toastSuccess(res && res.chunks > 0 ? `Re-indexed ${res.chunks} chunks across ${res.docs} documents` : 'Nothing to re-index')
      loadDocs()
    } catch (e) {
      logger.warn(e)
      const embeddingMissing = (e instanceof ApiError && e.code === 'EMBEDDING_NOT_CONFIGURED') || /embedding provider/i.test(String(e))
      toastError(embeddingMissing ? 'Re-index needs an embedding-capable provider key' : 'Re-index failed')
    } finally {
      setReindexing(false)
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
      <h2 className="text-xl font-semibold text-text-primary">{t('knowledge.title')}</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">
        Organization: <span className="font-medium text-text-primary">{activeOrg?.name}</span>
        <br />
        Upload organizational guidelines, SOPs, or coding standards. The AI will use these to contextualize its
        analysis.
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
                {selectedFile ? selectedFile.name : 'Select text or markdown file...'}
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
              {uploading ? <Loader2 className="w-4 h-4 animate-spin" /> : 'Index Document'}
            </Button>
          </div>
          <p className="text-2xs text-text-tertiary mt-2">Currently supports .txt and .md files up to 1MB.</p>
          {uploadError && <p className="text-xs text-red-400 mt-2">{uploadError}</p>}
        </div>
      )}

      <div className="space-y-3">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-medium text-text-primary">{t('knowledge.indexedTitle')}</h3>
          {canManage && docs.length > 0 && (
            <button
              onClick={handleReindex}
              disabled={reindexing}
              className="flex items-center gap-1.5 px-2.5 py-1 text-xs text-text-secondary hover:text-brand-400 border border-border-default hover:border-brand-500/50 rounded-md transition-colors disabled:opacity-50"
              title="Re-embed every document with the currently configured embedding provider (recovery after changing the embedding model)"
            >
              {reindexing ? <Loader2 className="w-3.5 h-3.5 animate-spin" /> : <RefreshCw className="w-3.5 h-3.5" />}
              {reindexing ? 'Re-indexing…' : 'Re-index'}
            </button>
          )}
        </div>
        {loading ? (
          <div className="flex justify-center p-8">
            <Loader2 className="w-6 h-6 animate-spin text-brand-500" />
          </div>
        ) : loadError ? (
          <ErrorState message={loadError} onRetry={loadDocs} />
        ) : docs.length === 0 ? (
          <div className="text-center py-12 bg-surface-1 rounded-lg border border-dashed border-border-default text-text-tertiary">
            No documents indexed yet.
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
                      Indexed {relativeTime(doc.createdAt)}
                      {doc.chunkCount != null && doc.chunkCount > 0 && (
                        <> · {doc.chunkCount} chunks{doc.vectorIndexed != null && ` (${doc.vectorIndexed} searchable)`}</>
                      )}
                    </div>
                    {(doc.chunkCount ?? 0) > 0 && (doc.vectorIndexed ?? 0) === 0 && (
                      <div className="text-xs text-amber-400">
                        Not searchable — embedding dimension mismatch; re-index below.
                      </div>
                    )}
                  </div>
                </div>
                {canManage && (
                  <button
                    onClick={() => handleDelete(doc.id)}
                    className="p-2 text-text-tertiary hover:text-red-400 opacity-0 group-hover:opacity-100 transition-opacity"
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
