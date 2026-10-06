import {getErrorMessage} from '@/lib/getErrorMessage'
import {useCallback, useEffect, useMemo, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {libraryApi, type LibraryFlow} from '@/api/library'
import {flowApi} from '@/api/flow'
import {useLibraryBrowseStore} from '@/stores/libraryBrowseStore'
import {useOrgStore} from '@/stores/orgStore'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import {useToast, useConfirm} from '@/components/shared'
import type {FlowDocument, PagedResponse} from '@/types'
import {logger} from '@/lib/logger'
import {SEARCH_DEBOUNCE_MS} from '@/lib/constants'
import LibraryFilterRail from './LibraryFilterRail'
import LibraryToolbar from './LibraryToolbar'
import LibraryGrid from './LibraryGrid'
import LibraryList from './LibraryList'
import LibraryDetailPanel from './LibraryDetailPanel'
import LibraryPager from './LibraryPager'
import LibrarySkeleton from './LibrarySkeleton'
import LibraryEmptyState from './LibraryEmptyState'
import {useLibraryWriteErrorToast} from './useLibraryWriteErrorToast'

// mergeTags folds the tags on newly loaded flows into the session's known set,
// returning the previous array when nothing new appeared so the filter rail
// does not re-render on every page load.
function mergeTags(known: string[], flows: LibraryFlow[]): string[] {
  const next = new Set(known)
  for (const f of flows) for (const tag of f.tags ?? []) next.add(tag)
  return next.size === known.length ? known : [...next].sort()
}

// Cloud-library browse workspace. Three-pane layout:
//   • filter rail (left)  — scope + per-org filter
//   • results pane (mid)  — search/sort/view-toggle toolbar + grid|list + pager
//   • detail panel (right) — populated when a flow is selected
//
// Opening a flow takes the same code path as LibraryTab: fetch content,
// hydrate flowStore, switch the main pane to 'block'.
export default function LibraryWorkspace() {
  const {t} = useTranslation('library')
  const view = useLibraryBrowseStore(s => s.view)
  const scope = useLibraryBrowseStore(s => s.scope)
  const selectedOrgIds = useLibraryBrowseStore(s => s.selectedOrgIds)
  const sort = useLibraryBrowseStore(s => s.sort)
  const query = useLibraryBrowseStore(s => s.query)
  const selectedTags = useLibraryBrowseStore(s => s.selectedTags)
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
  const reportWriteError = useLibraryWriteErrorToast()
  const {confirm, prompt} = useConfirm()
  const [knownTags, setKnownTags] = useState<string[]>([])
  // Bumped after a write (or a conflict reload) so the detail panel re-reads
  // the flow: it holds its own copy, and a stale version there turns the next
  // rename into a 409.
  const [detailReload, setDetailReload] = useState(0)

  const [items, setItems] = useState<LibraryFlow[]>([])
  const [total, setTotal] = useState(0)
  const [isLoading, setIsLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const abortRef = useRef<AbortController | null>(null)
  const [debouncedQuery, setDebouncedQuery] = useState(query)
  const [openingId, setOpeningId] = useState<string | null>(null)
  useEffect(() => {
    const t = setTimeout(() => setDebouncedQuery(query.trim()), SEARCH_DEBOUNCE_MS)
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
            tags: selectedTags.size > 0 ? [...selectedTags] : undefined,
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
        setKnownTags(prev => mergeTags(prev, res.items))
      } catch (e) {
        // Abort (superseded fetch or unmount) is intentional, not an error.
        if (ac.signal.aborted || (e instanceof DOMException && e.name === 'AbortError')) return
        const msg = getErrorMessage(e)
        logger.warn('Library: list failed', e)
        setError(msg)
      } finally {
        if (!ac.signal.aborted) setIsLoading(false)
      }
    },
    [scope, sort, requestOrgId, debouncedQuery, selectedTags, page, pageSize, selectedOrgIds],
  )

  useEffect(() => {
    // Kicks off the paged fetch (external system).
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void fetchPage(false)
    return () => abortRef.current?.abort()
  }, [fetchPage])

  const handleOpen = useCallback(
    async (flow: LibraryFlow) => {
      // Per-card loading (U4.5): the clicked card shows a spinner while the
      // content fetch runs — the click used to feel ignored.
      setOpeningId(flow.id)
      try {
        const doc = await libraryApi.getContent(flow.id)
        setDocument(doc as FlowDocument)
        useFlowStore.setState({libraryFlowId: flow.id, libraryVersion: flow.version, readOnly: !flow.canEdit})
        setMainPaneView('block')
      } catch (e) {
        toast.error(t('toasts.openFailed'), {description: getErrorMessage(e)})
      } finally {
        setOpeningId(null)
      }
    },
    [setDocument, setMainPaneView, toast, t],
  )

  const handleDelete = useCallback(
    async (flow: LibraryFlow) => {
      const ok = await confirm({
        title: t('toasts.deleteTitle'),
        message: t('toasts.deleteMessage', {name: flow.name}),
        danger: true,
        confirmLabel: t('toasts.delete'),
      })
      if (!ok) return
      try {
        await libraryApi.delete(flow.id)
        setItems(prev => prev.filter(f => f.id !== flow.id))
        setTotal(t => Math.max(0, t - 1))
        if (selectedFlowId === flow.id) setSelectedFlow(null)
        toast.success(t('toasts.deleted'))
      } catch (e) {
        toast.error(t('toasts.deleteFailed'), {description: getErrorMessage(e)})
      }
    },
    [selectedFlowId, setSelectedFlow, toast, confirm, t],
  )

  // A conflict reload re-reads both the page and the detail panel, so the next
  // write goes out with the version the server now holds.
  const reloadAfterConflict = useCallback(() => {
    setDetailReload(n => n + 1)
    void fetchPage(true)
  }, [fetchPage])

  // R0-4: rename a library flow. The backend's update endpoint always
  // accepted {name}; no UI ever sent it — renaming meant delete + re-upload.
  const handleRename = useCallback(
    async (flow: LibraryFlow) => {
      const name = await prompt({
        title: t('toasts.renameTitle'),
        label: t('toasts.renameLabel'),
        initialValue: flow.name,
        placeholder: t('toasts.renamePlaceholder'),
        confirmLabel: t('toasts.rename'),
      })
      if (!name?.trim() || name.trim() === flow.name) return
      try {
        const updated = await libraryApi.update(flow.id, {name: name.trim(), version: flow.version})
        setItems(prev => prev.map(f => (f.id === flow.id ? updated : f)))
        setDetailReload(n => n + 1)
        toast.success(t('toasts.renamed'))
      } catch (e) {
        reportWriteError(e, t('toasts.renameFailed'), reloadAfterConflict)
      }
    },
    [toast, prompt, reportWriteError, reloadAfterConflict, t],
  )

  // R0-4: duplicate = load content, save as "<name> (copy)" (same org).
  const handleDuplicate = useCallback(
    async (flow: LibraryFlow) => {
      try {
        const content = await libraryApi.getContent(flow.id)
        const created = await libraryApi.create({
          name: `${flow.name} (copy)`,
          orgId: flow.organizationId ?? undefined,
          content,
        })
        setItems(prev => [created, ...prev])
        setTotal(t => t + 1)
        toast.success(t('toasts.duplicatedAs', {name: created.name}))
      } catch (e) {
        toast.error(t('toasts.duplicateFailed'), {description: getErrorMessage(e)})
      }
    },
    [toast, t],
  )

  // Tag editor state (R2-4b): inline chip editing in the detail panel.
  const [editingTags, setEditingTags] = useState(false)
  const handleSaveTags = useCallback(
    async (flow: LibraryFlow, tags: string[]) => {
      try {
        const res = await flowApi.setTags(flow.id, tags)
        const saved = res?.tags ?? []
        setItems(prev => prev.map(f => (f.id === flow.id ? {...f, tags: saved} : f)))
        setKnownTags(prev => mergeTags(prev, [{...flow, tags: saved}]))
        setDetailReload(n => n + 1)
        toast.success(t('toasts.tagsUpdated'))
      } catch (e) {
        reportWriteError(e, t('toasts.tagsUpdateFailed'), reloadAfterConflict)
      }
    },
    [toast, reportWriteError, reloadAfterConflict, t],
  )

  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const showEmpty = !isLoading && items.length === 0
  const showSkeleton = isLoading && items.length === 0

  return (
    // The main pane's toolbar (section switcher, title) sits above this, the
    // same one every other system view has; the library used to draw its own
    // header with a back link, which made switching sections look like
    // leaving the app.
    <div className="library-workspace flex flex-col h-full bg-surface-1">
      {/* relative: the detail panel slides over this box when there is no
          room to dock it (see .library-detail). */}
      <div className="relative flex-1 flex overflow-hidden">
        <aside className="library-rail border-r border-border-subtle overflow-y-auto shrink-0">
          <LibraryFilterRail orgs={orgs} tags={knownTags} />
        </aside>

        <section className="flex-1 flex flex-col overflow-hidden min-w-0">
          <LibraryToolbar total={total} isLoading={isLoading} onRefresh={() => fetchPage(true)} />

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
                hasOrgFilter={(selectedOrgIds !== null && selectedOrgIds.size > 0) || selectedTags.size > 0}
              />
            ) : view === 'grid' ? (
              <LibraryGrid
                openingId={openingId}
                items={items}
                selectedId={selectedFlowId}
                onSelect={f => setSelectedFlow(f.id)}
                onOpen={handleOpen}
              />
            ) : (
              <LibraryList
                openingId={openingId}
                items={items}
                selectedId={selectedFlowId}
                onSelect={f => setSelectedFlow(f.id)}
                onOpen={handleOpen}
              />
            )}
          </div>

          {total > pageSize && (
            <div className="border-t border-border-subtle px-4 py-2 shrink-0">
              <LibraryPager page={page} totalPages={totalPages} total={total} onChange={setPage} />
            </div>
          )}
        </section>

        <aside
          className="library-detail shrink-0 overflow-y-auto"
          data-open={selectedFlowId ? 'true' : 'false'}
          aria-label={t('detail.panelAria')}
        >
          <LibraryDetailPanel
            flowId={selectedFlowId}
            reloadKey={detailReload}
            onOpen={handleOpen}
            onDelete={handleDelete}
            onRename={handleRename}
            onDuplicate={handleDuplicate}
            onSaveTags={handleSaveTags}
            editingTags={editingTags}
            setEditingTags={setEditingTags}
            onClose={() => setSelectedFlow(null)}
          />
        </aside>
      </div>
    </div>
  )
}
