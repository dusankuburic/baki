import {useCallback, useMemo, useRef, useEffect} from 'react'
import {useSearchStore} from '@/stores/searchStore'
import {useFlowStore} from '@/stores/flowStore'
import {useDebouncedSearch} from '@/hooks/useDebouncedSearch'
import {flowApi} from '@/api'
import type {SearchResult} from '@/types'

export function useSidebarSearch() {
  const searchQuery = useSearchStore(s => s.query)
  const setQuery = useSearchStore(s => s.setQuery)
  const searchResults = useSearchStore(s => s.results)
  const setResults = useSearchStore(s => s.setResults)
  const activeResultIndex = useSearchStore(s => s.activeResultIndex)
  const nextResult = useSearchStore(s => s.nextResult)
  const prevResult = useSearchStore(s => s.prevResult)
  const selectBlock = useFlowStore(s => s.selectBlock)
  const selectSubflow = useFlowStore(s => s.selectSubflow)
  const searchVersionRef = useRef(0)

  const handleSearch = useCallback(
    async (q: string) => {
      const doc = useFlowStore.getState().document
      if (!doc || !q) {
        searchVersionRef.current++
        setResults([], 0)
        return
      }
      const docId = doc.id
      const version = ++searchVersionRef.current
      try {
        const results = await flowApi.searchFlow(docId, {
          text: q,
          fuzzy: true,
          maxResults: 100,
        })
        if (version !== searchVersionRef.current) return
        if (useFlowStore.getState().document?.id !== docId) return
        if (results?.results) {
          setResults(results.results as SearchResult[], results.totalCount ?? 0)
        }
      } catch {
        if (version !== searchVersionRef.current) return
        if (useFlowStore.getState().document?.id !== docId) return
        setResults([], 0)
      }
    },
    [setResults],
  )

  const {search: debouncedSearch} = useDebouncedSearch({onSearch: handleSearch})

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
