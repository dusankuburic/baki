import React, { useState, useEffect, useRef, useCallback } from 'react'
import { Library, Search, Trash2, FolderOpen, Save, RefreshCw } from 'lucide-react'
import { libraryApi, type LibraryFlow } from '@/api/library'
import { VersionConflictError } from '@/api/client'
import { useFlowStore } from '@/stores/flowStore'
import { useUIStore } from '@/stores/uiStore'
import { useOrgStore } from '@/stores/orgStore'
import { Spinner, useToast } from '@/components/shared'
import type { FlowDocument } from '@/types'
import {logger} from '@/lib/logger'

export default function LibraryTab() {
  const [flows, setFlows] = useState<LibraryFlow[]>([])
  const [isLoading, setIsLoading] = useState(false)
  const [search, setSearch] = useState('')
  const [isSaving, setIsSaving] = useState(false)
  const setDocument = useFlowStore(s => s.setDocument)
  const currentDoc = useFlowStore(s => s.document)
  const libraryFlowId = useFlowStore(s => s.libraryFlowId)
  const libraryVersion = useFlowStore(s => s.libraryVersion)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const activeOrgId = useOrgStore(s => s.activeOrgId)
  const toast = useToast()
  const abortRef = useRef<AbortController | null>(null)

  const fetchLibrary = useCallback(async () => {
    abortRef.current?.abort()
    const ac = new AbortController()
    abortRef.current = ac
    setIsLoading(true)
    try {
      const page = await libraryApi.list({ query: search, orgId: activeOrgId ?? undefined })
      if (ac.signal.aborted) return
      setFlows(page.items)
    } catch (err) {
      if (ac.signal.aborted) return
      logger.warn('Failed to fetch library', err)
    } finally {
      if (!ac.signal.aborted) {
        setIsLoading(false)
      }
    }
  }, [search, activeOrgId])

  useEffect(() => {
    fetchLibrary()
    return () => { abortRef.current?.abort() }
  }, [fetchLibrary])

  const handleOpen = async (id: string) => {
    try {
      const fullDoc = await libraryApi.getContent(id)
      const meta = flows.find(f => f.id === id)
      setDocument(fullDoc as FlowDocument)
      useFlowStore.setState({
        libraryFlowId: id,
        libraryVersion: meta?.version ?? 0,
      })
      setMainPaneView('block')
    } catch (err) {
      toast.error('Failed to load flow', {
        description: err instanceof Error ? err.message : 'Unknown error',
      })
    }
  }

  const handleDelete = async (e: React.MouseEvent, id: string) => {
    e.stopPropagation()
    if (!confirm('Are you sure you want to delete this flow from the library?')) return
    try {
      await libraryApi.delete(id)
      setFlows(prev => prev.filter(f => f.id !== id))
    } catch (err) {
      toast.error('Failed to delete', {
        description: err instanceof Error ? err.message : 'Unknown error',
      })
    }
  }

  const reloadFromLibrary = async (id: string) => {
    try {
      const [fullDoc, meta] = await Promise.all([
        libraryApi.getContent(id),
        libraryApi.get(id),
      ])
      setDocument(fullDoc as FlowDocument)
      useFlowStore.setState({ libraryVersion: meta.version })
      toast.success('Reloaded latest version')
    } catch (err) {
      toast.error('Failed to reload', {
        description: err instanceof Error ? err.message : 'Unknown error',
      })
    }
  }

  const handleSaveCurrent = async () => {
    if (!currentDoc) return

    if (libraryFlowId) {
      setIsSaving(true)
      try {
        const updated = await libraryApi.update(libraryFlowId, {
          content: currentDoc,
          version: libraryVersion,
        })
        useFlowStore.setState({ libraryVersion: updated.version })
        toast.success('Flow updated')
        fetchLibrary()
      } catch (err) {
        if (err instanceof VersionConflictError) {
          toast.warning('Flow was modified by another user', {
            description: 'Reload the latest version to see their changes.',
            duration: 15000,
            action: {
              label: 'Reload',
              onClick: () => reloadFromLibrary(libraryFlowId),
            },
          })
        } else {
          toast.error('Failed to update flow', {
            description: err instanceof Error ? err.message : 'Unknown error',
          })
        }
      } finally {
        setIsSaving(false)
      }
      return
    }

    const name = prompt('Enter name for the library flow:', currentDoc.name)
    if (!name) return

    setIsSaving(true)
    try {
      const created = await libraryApi.create({
        name,
        orgId: activeOrgId ?? undefined,
        content: currentDoc as FlowDocument,
      })
      useFlowStore.setState({
        libraryFlowId: created.id,
        libraryVersion: created.version,
      })
      toast.success('Flow saved to library')
      fetchLibrary()
    } catch (err) {
      toast.error('Failed to save to library', {
        description: err instanceof Error ? err.message : 'Unknown error',
      })
    } finally {
      setIsSaving(false)
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
            disabled={isSaving}
            className="flex items-center justify-center gap-1.5 w-full py-1.5 bg-brand-600 text-white rounded-md text-xs font-medium hover:bg-brand-700 transition-colors shadow-sm disabled:opacity-50"
          >
            {isSaving ? (
              <RefreshCw size={12} className="animate-spin" />
            ) : (
              <Save size={12} />
            )}
            {libraryFlowId ? 'Update library flow' : 'Save current to library'}
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
                  {(f.canDelete ?? !f.isSharedWithMe) && (
                    <button
                      onClick={(e) => handleDelete(e, f.id)}
                      className="p-1 text-text-tertiary hover:text-red-500 rounded opacity-0 group-hover:opacity-100 transition-opacity"
                    >
                      <Trash2 size={12} />
                    </button>
                  )}
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
