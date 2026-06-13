import {useState, useEffect, useMemo} from 'react'
import {useOrgStore} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {request} from '@/api/client'
import {Trash2, FileText, Loader2, Upload} from 'lucide-react'
import Button from '@/components/shared/Button'
import {logger} from '@/lib/logger'

interface KnowledgeDoc {
  id: string
  filename: string
  createdAt: string
}

export default function KnowledgeBasePanel() {
  const {organisations, activeOrgId} = useOrgStore()
  const currentUser = useAuthStore(s => s.user)
  const activeOrg = useMemo(() => 
    organisations.find(o => o.id === activeOrgId), 
    [organisations, activeOrgId]
  )
  const canManage = useMemo(() => {
    if (!activeOrg || !currentUser) return false
    const membership = activeOrg.members.find(m => m.userId === currentUser.id)
    return membership?.role === 'admin' || membership?.role === 'member'
  }, [activeOrg, currentUser])
  
  const [docs, setDocs] = useState<KnowledgeDoc[]>([])
  const [loading, setLoading] = useState(false)
  const [uploading, setUploading] = useState(false)
  const [uploadError, setUploadError] = useState<string | null>(null)
  const [selectedFile, setSelectedFile] = useState<File | null>(null)
  const maxFileSizeMB = useSettingsStore(s => s.settings.parser.maxFileSizeMB)

  const loadDocs = async () => {
    if (!activeOrgId) return
    setLoading(true)
    try {
      const res = await request<KnowledgeDoc[]>(`/api/orgs/${activeOrgId}/knowledge`)
      setDocs(res || [])
    } catch (e) {
      logger.warn(e)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    let cancelled = false
    if (!activeOrgId) return
    setLoading(true)
    request<KnowledgeDoc[]>(`/api/orgs/${activeOrgId}/knowledge`)
      .then(res => { if (!cancelled) setDocs(res || []) })
      .catch(e => { if (!cancelled) logger.warn(e) })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [activeOrgId])

  const handleUpload = async () => {
    if (!activeOrgId || !selectedFile) return
    setUploadError(null)
    const maxBytes = maxFileSizeMB * 1024 * 1024
    if (selectedFile.size > maxBytes) {
      setUploadError(`File is ${(selectedFile.size / 1024 / 1024).toFixed(1)}MB — limit is ${maxFileSizeMB}MB`)
      return
    }
    setUploading(true)
    try {
      const content = await selectedFile.text()
      await request(`/api/orgs/${activeOrgId}/knowledge/upload`, {
        filename: selectedFile.name,
        content
      })
      setSelectedFile(null)
      loadDocs()
    } catch (e) {
      setUploadError('Upload failed: ' + e)
    } finally {
      setUploading(false)
    }
  }

  const handleDelete = async (docId: string) => {
    if (!activeOrgId || !confirm('Delete this document?')) return
    try {
      await request(`/api/orgs/${activeOrgId}/knowledge/${docId}`, {}, 'DELETE')
      loadDocs()
    } catch (e) {
      logger.warn(e)
    }
  }

  if (!activeOrgId) {
    return (
      <div className="flex flex-col items-center justify-center h-64 text-text-tertiary text-center px-4">
        <p>Please select an organization in the 'Organizations' tab first to manage its knowledge base.</p>
      </div>
    )
  }

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">Knowledge Base (RAG)</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">
        Organization: <span className="font-medium text-text-primary">{activeOrg?.name}</span>
        <br />
        Upload organizational guidelines, SOPs, or coding standards. The AI will use these to contextualize its analysis.
      </p>

      {canManage && (
        <div className="bg-surface-2 rounded-lg border border-border-default p-4 mb-8">
          <h3 className="text-sm font-medium text-text-primary mb-3">Upload SOP</h3>
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
                onChange={e => { setSelectedFile(e.target.files?.[0] || null); setUploadError(null) }}
              />
            </label>
            <Button 
              variant="primary" 
              disabled={!selectedFile || uploading}
              onClick={handleUpload}
            >
              {uploading ? <Loader2 className="w-4 h-4 animate-spin" /> : 'Index Document'}
            </Button>
          </div>
          <p className="text-2xs text-text-tertiary mt-2">
            Currently supports .txt and .md files up to {maxFileSizeMB}MB.
          </p>
          {uploadError && (
            <p className="text-xs text-red-400 mt-2">{uploadError}</p>
          )}
        </div>
      )}

      <div className="space-y-3">
        <h3 className="text-sm font-medium text-text-primary">Indexed Documents</h3>
        {loading ? (
          <div className="flex justify-center p-8"><Loader2 className="w-6 h-6 animate-spin text-brand-500" /></div>
        ) : docs.length === 0 ? (
          <div className="text-center py-12 bg-surface-1 rounded-lg border border-dashed border-border-default text-text-tertiary">
            No documents indexed yet.
          </div>
        ) : (
          <div className="grid gap-2">
            {docs.map(doc => (
              <div key={doc.id} className="flex items-center justify-between p-3 bg-surface-2 border border-border-default rounded-lg group hover:border-brand-500/50 transition-colors">
                <div className="flex items-center gap-3">
                  <div className="p-2 bg-brand-500/10 rounded">
                    <FileText className="w-5 h-5 text-brand-500" />
                  </div>
                  <div>
                    <div className="text-sm font-medium text-text-primary">{doc.filename}</div>
                    <div className="text-xs text-text-tertiary">Indexed on {new Date(doc.createdAt).toLocaleDateString()}</div>
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
