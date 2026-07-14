import {useEffect, useRef, useCallback} from 'react'

type UseDebouncedSearchOptions = {
  delay?: number
  onSearch: (query: string) => void
}

export function useDebouncedSearch({delay = 150, onSearch}: UseDebouncedSearchOptions) {
  const timerRef = useRef<ReturnType<typeof setTimeout>>()
  const onSearchRef = useRef(onSearch)
  useEffect(() => {
    onSearchRef.current = onSearch
  })

  const search = useCallback(
    (query: string) => {
      if (timerRef.current) {
        clearTimeout(timerRef.current)
      }

      const trimmed = query.trim()
      if (trimmed.length === 0) {
        onSearchRef.current('')
        return
      }

      timerRef.current = setTimeout(() => {
        onSearchRef.current(trimmed)
      }, delay)
    },
    [delay],
  )

  useEffect(() => {
    return () => {
      if (timerRef.current) {
        clearTimeout(timerRef.current)
      }
    }
  }, [])

  return {search}
}
