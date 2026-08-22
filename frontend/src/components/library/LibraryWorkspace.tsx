import {useCallback, useEffect, useMemo, useRef, useState} from 'react'
import {RefreshCw, ChevronLeft} from 'lucide-react'
import {libraryApi, type LibraryFlow} from '@/api/library'
import {useLibraryBrowseStore} from '@/stores/libraryBrowseStore'
import {useOrgStore} from '@/stores/orgStore'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import {Button, useToast, useConfirm} from '@/components/shared'
import type {FlowDocument, PagedResponse} from '@/types'
import {logger} from '@/lib/logger'
import LibraryFilterRail from './LibraryFilterRail'
import LibraryToolbar from './LibraryToolbar'
import LibraryGrid from './LibraryGrid'
import LibraryList from './LibraryList'
import LibraryDetailPanel from './LibraryDetailPanel'
import LibraryPager from './LibraryPager'
import LibrarySkeleton from './LibrarySkeleton'
import LibraryEmptyState from './LibraryEmptyState'

// Cloud-library browse workspace. Three-pane layout:
//   • filter rail (left)  — scope + per-org filter
//   • results pane (mid)  — search/sort/view-toggle toolbar + grid|list + pager
//   • detail panel (right) — populated when a flow is selected
//
// Opening a flow takes the same code path as LibraryTab: fetch content,
// hydrate flowStore, switch the main pane to 'block'.
export default function LibraryWorkspace() {
  const view = useLibraryBrowseStore(s => s.view)
  const scope = useLibraryBrowseStore(s => s.scope)
  const selectedOrgIds = useLibraryBrowseStore(s => s.selectedOrgIds)
  const sort = useLibraryBrowseStore(s => s.sort)
  const query = useLibraryBrowseStore(s => s.query)
  const page = useLibraryBrowseStore(s => s.page)
  const pageSize = useLibraryBrowseStore(s => s.pageSize)
  const selectedFlowId = useLibraryBrowseStore(s => s.selectedFlowId)
  const setSelectedFlow = useLibraryBrowseStore(s => s.setSelectedFlow)
  const setPage = useLibraryBrowseStore(s => s.setPage)

  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const setDocument = useFlowStore(s => s.setDocument)
  const orgs = useOrgStore(s => s.organisations)
  const loadOrgs = useOrgStore(s => s.loadOrgs)
  const toast = useToast()
  const {confirm} = useConfirm()

  const [items, setItems] = useState<LibraryFlow[]>([])
  const [total, setTotal] = useState(0)
  const [isLoading, setIsLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const abortRef = useRef<AbortController | null>(null)
  const [debouncedQuery, setDebouncedQuery] = useState(query)
  useEffect(() => {
    const t = setTimeout(() => setDebouncedQuery(query.trim()), 250)
    return () => clearTimeout(t)
  }, [query])

  useEffect(() => {
    // Ensure the org list is hydrated so the filter rail can show membership chips.
    loadOrgs().catch(() => {
      /* surfaced by orgStore.error */
    })
  }, [loadOrgs])

  // Resolve "all orgs the user belongs to" → request-level orgId filter. When
  // exactly one org (or "personal" = empty string) is selected we can pass it
  // server-side as `orgId`; for the "all" or multi-org cases the server's
  // scope=all already widens to every membership, so we rely on that and
  // post-filter locally for the multi-org chip subset. Keeps the request
  // surface narrow without rebuilding the storage filter for chip toggles.
  const requestOrgId = useMemo(() => {
    if (!selectedOrgIds) return undefined
    if (selectedOrgIds.size !== 1) return undefined
    const only = [...selectedOrgIds][0]
    // Empty-string id (the personal bucket) cannot be passed as orgId — the
    // server would treat that as "no org filter". Fall back to client-side.
    return only ? only : undefined
  }, [selectedOrgIds])

  const fetchPage = useCallback(
    async (silent: boolean) => {
      abortRef.current?.abort()
      const ac = new AbortController()
      abortRef.current = ac
      if (!silent) setIsLoading(true)
      setError(null)
      try {
        const res: PagedResponse<LibraryFlow> = await libraryApi.list(
          {
            scope,
            sort,
            orgId: requestOrgId,
            query: debouncedQuery || undefined,
            limit: pageSize,
            offset: page * pageSize,
          },
          // Real cancellation: rapid filter/search typing aborts the previous
          // request in-flight instead of discarding its late result.
          ac.signal,
        )
        if (ac.signal.aborted) return
        // Multi-org chip subset filtering happens client-side over the page —
        // the server doesn't accept a list of orgIds, only a single one.
        let pageItems = res.items
        if (selectedOrgIds && selectedOrgIds.size > 1) {
          pageItems = pageItems.filter(f => selectedOrgIds.has(f.organizationId ?? ''))
        }
        setItems(pageItems)
        setTotal(res.total)
      } catch (e) {
        // Abort (superseded fetch or unmount) is intentional, not an error.
        if (ac.signal.aborted || (e instanceof DOMException && e.name === 'AbortError')) return
        const msg = e instanceof Error ? e.message : String(e)
        logger.warn('Library: list failed', e)
        setError(msg)
      } finally {
        if (!ac.signal.aborted) setIsLoading(false)
      }
    },
    [scope, sort, requestOrgId, debouncedQuery, page, pageSize, selectedOrgIds],
  )

  useEffect(() => {
    void fetchPage(false)
    return () => abortRef.current?.abort()
  }, [fetchPage])

  const handleOpen = useCallback(
    async (flow: LibraryFlow) => {
      try {
        const doc = await libraryApi.getContent(flow.id)
        setDocument(doc as FlowDocument)
        useFlowStore.setState({libraryFlowId: flow.id, libraryVersion: flow.version})
        setMainPaneView('block')
      } catch (e) {
        toast.error('Failed to load flow', {description: e instanceof Error ? e.message : String(e)})
      }
    },
    [setDocument, setMainPaneView, toast],
  )

  const handleDelete = useCallback(
    async (flow: LibraryFlow) => {
      const ok = await confirm({
        title: 'Delete flow',
        message: `Delete "${flow.name}" from the cloud library? This cannot be undone.`,
        danger: true,
        confirmLabel: 'Delete',
      })
      if (!ok) return
      try {
        await libraryApi.delete(flow.id)
        setItems(prev => prev.filter(f => f.id !== flow.id))
        setTotal(t => Math.max(0, t - 1))
        if (selectedFlowId === flow.id) setSelectedFlow(null)
        toast.success('Flow deleted')
      } catch (e) {
        toast.error('Failed to delete', {description: e instanceof Error ? e.message : String(e)})
      }
    },
    [selectedFlowId, setSelectedFlow, toast, confirm],
  )

  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const showEmpty = !isLoading && items.length === 0
  const showSkeleton = isLoading && items.length === 0

  return (
    <div className="flex flex-col h-full bg-surface-1">
      <header className="flex items-center justify-between gap-3 px-4 h-12 border-b border-border-subtle flex-shrink-0">
        <div className="flex items-center gap-3 min-w-0">
          <Button variant="ghost" size="sm" icon={ChevronLeft} onClick={() => setMainPaneView('home')}>
            Home
          </Button>
          <h1 className="text-sm font-semibold text-text-primary truncate">Cloud Library</h1>
          <span className="text-xs text-text-tertiary">
            {isLoading ? 'loading…' : `${total} flow${total === 1 ? '' : 's'}`}
          </span>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="ghost" size="sm" icon={RefreshCw} onClick={() => fetchPage(true)} disabled={isLoading}>
            Refresh
          </Button>
        </div>
      </header>

      <div className="flex-1 flex overflow-hidden">
        <aside className="w-56 border-r border-border-subtle overflow-y-auto flex-shrink-0">
          <LibraryFilterRail orgs={orgs} />
        </aside>

        <section className="flex-1 flex flex-col overflow-hidden min-w-0">
          <LibraryToolbar />

          {error && (
            <div className="mx-4 my-3 px-3 py-2 text-sm bg-semantic-error/10 border border-semantic-error/30 rounded-lg text-semantic-error">
              {error}
            </div>
          )}

          <div className="flex-1 overflow-y-auto p-4">
            {showSkeleton ? (
              <LibrarySkeleton view={view} />
            ) : showEmpty ? (
              <LibraryEmptyState
                scope={scope}
                hasQuery={debouncedQuery.length > 0}
                hasOrgFilter={selectedOrgIds !== null && selectedOrgIds.size > 0}
              />
            ) : view === 'grid' ? (
              <LibraryGrid
                items={items}
                selectedId={selectedFlowId}
                onSelect={f => setSelectedFlow(f.id)}
                onOpen={handleOpen}
              />
            ) : (
              <LibraryList
                items={items}
                selectedId={selectedFlowId}
                onSelect={f => setSelectedFlow(f.id)}
                onOpen={handleOpen}
              />
            )}
          </div>

          {total > pageSize && (
            <div className="border-t border-border-subtle px-4 py-2 flex-shrink-0">
              <LibraryPager page={page} totalPages={totalPages} total={total} onChange={setPage} />
            </div>
          )}
        </section>

        <aside className="w-80 border-l border-border-subtle overflow-y-auto flex-shrink-0 hidden xl:block">
          <LibraryDetailPanel
            flowId={selectedFlowId}
            onOpen={handleOpen}
            onDelete={handleDelete}
            onClose={() => setSelectedFlow(null)}
          />
        </aside>
      </div>
    </div>
  )
}
