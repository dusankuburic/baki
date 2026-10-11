import {useState, useCallback} from 'react'

interface UseListNavOptions {
  count: number
  onSelect: (index: number) => void
  onClose: () => void
  mode?: 'clamp' | 'wrap'
  extraSelectKeys?: string[]
}

interface UseListNavResult {
  activeIndex: number
  setActiveIndex: (i: number | ((prev: number) => number)) => void
  handleKeyDown: (e: {key: string; preventDefault: () => void}) => void
}

export function useListNavigation({
  count,
  onSelect,
  onClose,
  mode = 'clamp',
  extraSelectKeys = [],
}: UseListNavOptions): UseListNavResult {
  const [activeIndex, setActiveIndex] = useState(0)

  const handleKeyDown = useCallback(
    (e: {key: string; preventDefault: () => void}) => {
      const selectKeys = new Set(['Enter', ...extraSelectKeys])
      if (e.key === 'ArrowDown') {
        e.preventDefault()
        setActiveIndex(prev => {
          if (mode === 'wrap') return (prev + 1) % count
          return Math.min(prev + 1, count - 1)
        })
      } else if (e.key === 'ArrowUp') {
        e.preventDefault()
        setActiveIndex(prev => {
          if (mode === 'wrap') return (prev - 1 + count) % count
          return Math.max(prev - 1, 0)
        })
      } else if (selectKeys.has(e.key) && count > 0) {
        e.preventDefault()
        onSelect(activeIndex)
      } else if (e.key === 'Escape') {
        e.preventDefault()
        onClose()
      }
    },
    [count, activeIndex, onSelect, onClose, mode, extraSelectKeys],
  )

  return {activeIndex, setActiveIndex, handleKeyDown}
}
