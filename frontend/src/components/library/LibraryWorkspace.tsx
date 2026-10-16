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
import {EMPTY_TRAIL, MAX_LIST_OFFSET, recordPage, trailCursor, type PageTrail} from '@/lib/pageTrail'
import LibraryFilterRail from './LibraryFilterRail'
import LibraryToolbar from './LibraryToolbar'
import LibraryGrid from './LibraryGrid'
import LibraryList from './LibraryList'
import LibraryDetailPanel from './LibraryDetailPanel'
import LibraryPager from './LibraryPager'
import LibrarySkeleton from './LibrarySkeleton'
import LibraryEmptyState from './LibraryEmptyState'
import {useLibraryWriteErrorToast} from './useLibraryWriteErrorToast'

function mergeTags(known: string[], flows: LibraryFlow[]): string[] {
  const next = new Set(known)
  for (const f of flows) for (const tag of f.tags ?? []) next.add(tag)
  return next.size === known.length ? known : [...next].sort()
}

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
  const [detailReload, setDetailReload] = useState(0)

  const [items, setItems] = useState<LibraryFlow[]>([])
  const [total, setTotal] = useState(0)
  const [totalCapped, setTotalCapped] = useState(false)
  const [hasMore, setHasMore] = useState<boolean | undefined>(undefined)
  const [isLoading, setIsLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const abortRef = useRef<AbortController | null>(null)
  // The nextCursor each page answered, by the page it leads to, for the filters in cursorsKey: a page reached by its
  // cursor costs the server one index descent instead of skipping every earlier flow. Any filter change starts over
  // at page 0, which needs none; a server that predates the cursors answers none, and the offset alone pages.
  const trailRef = useRef<PageTrail>(EMPTY_TRAIL)
  // The last answer came from a server that predates the cursors (no hasMore): it has no offset cap either.
  const olderServerRef = useRef(false)
  // The page whose answer is shown (cursorsKey#page): until the page asked for answers, Next waits, since the cursor
  // of the page after it is not known yet.
  const [shownKey, setShownKey] = useState('')
  const [debouncedQuery, setDebouncedQuery] = useState(query)
  const [openingId, setOpeningId] = useState<string | null>(null)
  useEffect(() => {
    const t = setTimeout(() => setDebouncedQuery(query.trim()), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(t)
  }, [query])

  useEffect(() => {
    loadOrgs().catch(() => {
    })
  }, [loadOrgs])

  const requestOrgId = useMemo(() => {
    if (!selectedOrgIds) return undefined
    if (selectedOrgIds.size !== 1) return undefined
    const only = [...selectedOrgIds][0]
    return only ? only : undefined
  }, [selectedOrgIds])

  const tags = useMemo(() => (selectedTags.size > 0 ? [...selectedTags] : undefined), [selectedTags])
  const cursorsKey = useMemo(
    () => JSON.stringify([scope, sort, requestOrgId, debouncedQuery, tags, pageSize]),
    [scope, sort, requestOrgId, debouncedQuery, tags, pageSize],
  )

  const fetchPage = useCallback(
    async (silent: boolean, recount = false) => {
      abortRef.current?.abort()
      const ac = new AbortController()
      abortRef.current = ac
      if (!silent) setIsLoading(true)
      setError(null)
      const cursor = page > 0 ? trailCursor(trailRef.current, cursorsKey, page) : undefined
      if (!cursor && page * pageSize > MAX_LIST_OFFSET && !olderServerRef.current) {
        // No offset reaches this page (the server refuses one past the cap) and no cursor is known for it, as after
        // a remount: start over. A server that predates the cursors has no cap: it takes the offset.
        setPage(0)
        return
      }
      const filter = {scope, sort, orgId: requestOrgId, query: debouncedQuery || undefined, tags}
      // A page reached by cursor reports the total its first page counted: Refresh counts again beside it.
      const counted = recount && cursor ? libraryApi.list({...filter, limit: 1, offset: 0}, ac.signal) : undefined
      counted?.catch(() => {})
      try {
        const res: PagedResponse<LibraryFlow> = await libraryApi.list(
          {...filter, limit: pageSize, offset: page * pageSize, cursor},
          ac.signal,
        )
        if (ac.signal.aborted) return
        trailRef.current = recordPage(trailRef.current, cursorsKey, page, res.nextCursor)
        olderServerRef.current = res.hasMore === undefined
        let pageItems = res.items
        if (selectedOrgIds && selectedOrgIds.size > 1) {
          pageItems = pageItems.filter(f => selectedOrgIds.has(f.organizationId ?? ''))
        }
        setItems(pageItems)
        setShownKey(`${cursorsKey}#${page}`)
        // The count shown is the one this list last made (and adjusted since): a cursor's would undo that. A server
        // that predates the cursors counted afresh, without a cap.
        if (!cursor || res.hasMore === undefined) {
          setTotal(res.total)
          setTotalCapped(res.totalCapped === true)
        }
        // Past the offset cap only a cursor reaches the next page (a name sort has none).
        setHasMore(
          res.hasMore === undefined
            ? undefined
            : res.hasMore && (res.nextCursor !== undefined || (page + 1) * pageSize <= MAX_LIST_OFFSET),
        )
        setKnownTags(prev => mergeTags(prev, res.items))
        const fresh = await counted?.catch((e: unknown) => logger.warn('Library: recount failed', e))
        if (fresh && !ac.signal.aborted) {
          setTotal(fresh.total)
          setTotalCapped(fresh.totalCapped === true)
        }
      } catch (e) {
        if (ac.signal.aborted || (e instanceof DOMException && e.name === 'AbortError')) return
        const msg = getErrorMessage(e)
        logger.warn('Library: list failed', e)
        setError(msg)
      } finally {
        if (!ac.signal.aborted) setIsLoading(false)
      }
    },
    [scope, sort, requestOrgId, debouncedQuery, tags, cursorsKey, page, pageSize, selectedOrgIds, setPage],
  )

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void fetchPage(false)
    return () => abortRef.current?.abort()
  }, [fetchPage])

  const handleOpen = useCallback(
    async (flow: LibraryFlow) => {
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

  const reloadAfterConflict = useCallback(() => {
    setDetailReload(n => n + 1)
    void fetchPage(true)
  }, [fetchPage])

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

  // The count can trail the pages (a page reached by cursor keeps the one last made): never fewer pages than the one
  // shown and the one hasMore promises.
  const totalPages = Math.max(1, Math.ceil(total / pageSize), page + 1 + (hasMore ? 1 : 0))
  const settled = shownKey === `${cursorsKey}#${page}`
  const showEmpty = !isLoading && items.length === 0
  const showSkeleton = isLoading && items.length === 0

  return (
    <div className="library-workspace flex flex-col h-full bg-surface-1">
      <div className="relative flex-1 flex overflow-hidden">
        <aside className="library-rail border-r border-border-subtle overflow-y-auto shrink-0">
          <LibraryFilterRail orgs={orgs} tags={knownTags} />
        </aside>

        <section className="flex-1 flex flex-col overflow-hidden min-w-0">
          <LibraryToolbar
            total={total}
            totalCapped={totalCapped}
            isLoading={isLoading}
            onRefresh={() => fetchPage(true, true)}
          />

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

          {(total > pageSize || page > 0 || hasMore === true) && (
            <div className="border-t border-border-subtle px-4 py-2 shrink-0">
              <LibraryPager
                page={page}
                totalPages={totalPages}
                total={total}
                totalCapped={totalCapped}
                hasMore={hasMore}
                busy={!settled}
                onChange={setPage}
              />
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
