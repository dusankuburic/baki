import React, { useState, useEffect } from 'react'
import { Library, Search, Trash2, FolderOpen, Save } from 'lucide-react'
import { libraryApi, type LibraryFlow } from '@/api/library'
import { useFlowStore } from '@/stores/flowStore'
import { useUIStore } from '@/stores/uiStore'
import { Spinner } from '@/components/shared'

export default function LibraryTab() {
  const [flows, setFlows] = useState<LibraryFlow[]>([])
  const [isLoading, setIsLoading] = useState(false)
  const [search, setSearch] = useState('')
  const setDocument = useFlowStore(s => s.setDocument)
  const currentDoc = useFlowStore(s => s.document)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)

  const fetchLibrary = async () => {
    setIsLoading(true)
    try {
      const list = await libraryApi.list({ query: search })
      setFlows(list)
    } catch (err) {
      console.error('Failed to fetch library', err)
    } finally {
      setIsLoading(false)
    }
  }

  useEffect(() => {
    fetchLibrary()
  }, [search])

  const handleOpen = async (id: string) => {
    try {
      const fullDoc = await libraryApi.getContent(id)
      setDocument(fullDoc as any)
      setMainPaneView('block')
    } catch (err) {
      alert('Failed to load flow: ' + (err instanceof Error ? err.message : 'Unknown error'))
    }
  }

  const handleDelete = async (e: React.MouseEvent, id: string) => {
    e.stopPropagation()
    if (!confirm('Are you sure you want to delete this flow from the library?')) return
    try {
      await libraryApi.delete(id)
      setFlows(prev => prev.filter(f => f.id !== id))
    } catch (err) {
      alert('Failed to delete: ' + (err instanceof Error ? err.message : 'Unknown error'))
    }
  }

  const handleSaveCurrent = async () => {
    if (!currentDoc) return
    const name = prompt('Enter name for the library flow:', currentDoc.name)
    if (!name) return
    
    try {
      await libraryApi.create({
        name,
        content: currentDoc as any
      })
      fetchLibrary()
    } catch (err) {
      alert('Failed to save to library: ' + (err instanceof Error ? err.message : 'Unknown error'))
    }
  }

  return (
    <div className="flex flex-col h-full bg-surface-1">
      <div className="p-3 border-b border-border-subtle space-y-2">
        <div className="relative">
          <Search size={14} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-text-tertiary" />
          <input
            type="text"
            placeholder="Search library..."
            value={search}
            onChange={e => setSearch(e.target.value)}
            className="w-full bg-surface-2 border border-border-default rounded-md py-1.5 pl-8 pr-3 text-xs focus:outline-none focus:ring-1 focus:ring-brand-500"
          />
        </div>
        
        {currentDoc && (
          <button
            onClick={handleSaveCurrent}
            className="flex items-center justify-center gap-1.5 w-full py-1.5 bg-brand-600 text-white rounded-md text-xs font-medium hover:bg-brand-700 transition-colors shadow-sm"
          >
            <Save size={12} />
            Save current to library
          </button>
        )}
      </div>

      <div className="flex-1 overflow-y-auto">
        {isLoading ? (
          <div className="flex justify-center p-8">
            <Spinner size={20} />
          </div>
        ) : flows.length > 0 ? (
          <div className="divide-y divide-border-subtle/30">
            {flows.map(f => (
              <div
                key={f.id}
                onClick={() => handleOpen(f.id)}
                className="group p-3 hover:bg-surface-2 cursor-pointer transition-colors"
              >
                <div className="flex items-start justify-between gap-2">
                  <div className="flex-1 min-w-0">
                    <div className="text-xs font-semibold text-text-primary truncate">{f.name}</div>
                    {f.description && <div className="text-2xs text-text-tertiary truncate">{f.description}</div>}
                  </div>
                  <button
                    onClick={(e) => handleDelete(e, f.id)}
                    className="p-1 text-text-tertiary hover:text-red-500 rounded opacity-0 group-hover:opacity-100 transition-opacity"
                  >
                    <Trash2 size={12} />
                  </button>
                </div>
                <div className="mt-1.5 flex items-center gap-2 text-2xs text-text-muted">
                  <span className="flex items-center gap-0.5">
                    <FolderOpen size={10} />
                    {f.blockCount}
                  </span>
                  <span>·</span>
                  <span>{new Date(f.updatedAt).toLocaleDateString()}</span>
                  {f.isSharedWithMe && (
                    <>
                      <span>·</span>
                      <span className="text-blue-500 font-medium">Shared</span>
                    </>
                  )}
                </div>
              </div>
            ))}
          </div>
        ) : (
          <div className="flex flex-col items-center justify-center p-8 text-center text-text-tertiary">
            <Library size={24} className="mb-2 opacity-20" />
            <p className="text-xs">No flows found in your library.</p>
          </div>
        )}
      </div>
    </div>
  )
}
