import {useCallback, useMemo, useState} from 'react'
import type {ChatMessage} from '@/types'

export function useMessageSearch(activeThreadMessages: readonly ChatMessage[]) {
  const [msgSearch, setMsgSearch] = useState('')
  const [searchOpen, setSearchOpen] = useState(false)
  const [matchCursor, setMatchCursor] = useState<{query: string; index: number; nonce: number}>({
    query: '',
    index: 0,
    nonce: 0,
  })
  const toggleSearch = useCallback(() => setSearchOpen(v => !v), [])
  const closeSearch = useCallback(() => {
    setSearchOpen(false)
    setMsgSearch('')
  }, [])

  const searchQuery = searchOpen ? msgSearch.trim() : ''
  const matchIndices = useMemo(() => {
    if (!searchQuery) return []
    const q = searchQuery.toLowerCase()
    const out: number[] = []
    activeThreadMessages.forEach((m, i) => {
      if (m.content.toLowerCase().includes(q)) out.push(i)
    })
    return out
  }, [activeThreadMessages, searchQuery])

  const stepMatch = useCallback(
    (delta: number) => {
      setMatchCursor(prev => {
        const count = matchIndices.length
        if (count === 0) return prev
        const from = prev.query === searchQuery ? prev.index : 0
        return {query: searchQuery, index: (from + delta + count) % count, nonce: prev.nonce + 1}
      })
    },
    [matchIndices.length, searchQuery],
  )
  const nextMatch = useCallback(() => stepMatch(1), [stepMatch])
  const prevMatch = useCallback(() => stepMatch(-1), [stepMatch])

  const rawCursor = matchCursor.query === searchQuery ? matchCursor.index : 0
  const safeCursor = matchIndices.length > 0 ? Math.min(rawCursor, matchIndices.length - 1) : 0
  const activeMatchIndex = matchIndices.length > 0 ? (matchIndices[safeCursor] ?? -1) : -1
  const scrollTo = useMemo(
    () =>
      activeMatchIndex >= 0 && matchCursor.nonce > 0 ? {index: activeMatchIndex, nonce: matchCursor.nonce} : undefined,
    [activeMatchIndex, matchCursor.nonce],
  )

  return {
    msgSearch,
    setMsgSearch,
    searchOpen,
    toggleSearch,
    closeSearch,
    searchQuery,
    matchIndices,
    nextMatch,
    prevMatch,
    safeCursor,
    activeMatchIndex,
    scrollTo,
  }
}
