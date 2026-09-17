import {useCallback, useMemo, useEffect} from 'react'
import {useSearchStore} from '@/stores/searchStore'
import {useFlowStore} from '@/stores/flowStore'
import {useDebouncedSearch} from '@/hooks/useDebouncedSearch'
import {flowApi} from '@/api'
import type {SearchResult} from '@/types'

// Module-scoped so the sidebar's debounced typing and runSearch (callers
// outside the sidebar) share one staleness counter: whichever search started
// last wins, regardless of which path started it.
let searchVersion = 0

async function searchCurrentFlow(q: string): Promise<void> {
  const {setResults} = useSearchStore.getState()
  const doc = useFlowStore.getState().document
  if (!doc || !q) {
    searchVersion++
    setResults([], 0)
    return
  }
  const docId = doc.id
  const version = ++searchVersion
  try {
    const results = await flowApi.searchFlow(docId, {
      text: q,
      fuzzy: true,
      maxResults: 100,
    })
    if (version !== searchVersion) return
    if (useFlowStore.getState().document?.id !== docId) return
    if (results?.results) {
      setResults(results.results as SearchResult[], results.totalCount ?? 0)
    }
  } catch {
    if (version !== searchVersion) return
    if (useFlowStore.getState().document?.id !== docId) return
    setResults([], 0)
  }
}

// runSearch puts q in the sidebar search box AND runs the search immediately.
// BlockCard's "Find Usages" used to set searchStore.query alone: the box
// showed the text but nothing ever queried the flow, so the sidebar sat on a
// query with no results.
export function runSearch(q: string): Promise<void> {
  useSearchStore.getState().setQuery(q)
  return searchCurrentFlow(q.trim())
}

export function useSidebarSearch() {
  const searchQuery = useSearchStore(s => s.query)
  const setQuery = useSearchStore(s => s.setQuery)
  const searchResults = useSearchStore(s => s.results)
  const activeResultIndex = useSearchStore(s => s.activeResultIndex)
  const nextResult = useSearchStore(s => s.nextResult)
  const prevResult = useSearchStore(s => s.prevResult)
  const selectBlock = useFlowStore(s => s.selectBlock)
  const selectSubflow = useFlowStore(s => s.selectSubflow)

  const {search: debouncedSearch} = useDebouncedSearch({onSearch: searchCurrentFlow})

  const handleQueryChange = useCallback(
    (q: string) => {
      setQuery(q)
      debouncedSearch(q)
    },
    [setQuery, debouncedSearch],
  )

  // Navigate to the active search result in the flow tree
  useEffect(() => {
    if (searchResults.length === 0 || activeResultIndex < 0) return
    const result = searchResults[activeResultIndex]
    if (result) {
      selectBlock(result.blockId)
      selectSubflow(result.subflowId)
    }
  }, [activeResultIndex, searchResults, selectBlock, selectSubflow])

  const matchedBlockIds = useMemo(() => {
    if (searchResults.length === 0) return undefined
    return new Set(searchResults.map(r => r.blockId))
  }, [searchResults])

  const searchHighlightsMap = useMemo(() => {
    if (searchResults.length === 0) return undefined
    const map = new Map<string, {start: number; end: number}[]>()
    for (const r of searchResults) {
      if (r.highlights?.length) map.set(r.blockId, r.highlights)
    }
    return map.size > 0 ? map : undefined
  }, [searchResults])

  return {
    searchQuery,
    handleQueryChange,
    searchResults,
    matchedBlockIds,
    searchHighlightsMap,
    activeResultIndex,
    nextResult,
    prevResult,
  }
}
