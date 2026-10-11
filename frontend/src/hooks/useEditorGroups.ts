import {useMemo, useRef, useCallback} from 'react'
import {useEditorStore} from '@/stores/editorStore'

export function useEditorGroups() {
  const groups = useEditorStore(s => s.groups)
  const focusedGroupIndex = useEditorStore(s => s.focusedGroupIndex)
  const groupWidths = useEditorStore(s => s.groupWidths)
  const focusGroup = useEditorStore(s => s.focusGroup)
  const openInGroup = useEditorStore(s => s.openInGroup)
  const closeTab = useEditorStore(s => s.closeTab)
  const closeAllTabs = useEditorStore(s => s.closeAllTabs)
  const closeOtherTabs = useEditorStore(s => s.closeOtherTabs)
  const closeGroup = useEditorStore(s => s.closeGroup)
  const moveTabToGroup = useEditorStore(s => s.moveTabToGroup)
  const setGroupWidths = useEditorStore(s => s.setGroupWidths)
  const containerRef = useRef<HTMLDivElement>(null)
  const groupElsRef = useRef<Array<HTMLDivElement | null>>([])
  const pendingWidthsRef = useRef<number[] | null>(null)

  const registerGroup = useCallback(
    (gi: number) => (el: HTMLDivElement | null) => {
      groupElsRef.current[gi] = el
    },
    [],
  )

  const widths = useMemo(() => {
    if (groups.length <= 1) return [1]
    if (groupWidths.length === groups.length) return groupWidths
    return groups.map(() => 1 / groups.length)
  }, [groups, groupWidths])

  const handleColumnDrag = useCallback(
    (leftIndex: number, delta: number) => {
      if (groups.length < 2) return
      const containerWidth = containerRef.current?.clientWidth ?? 1
      const fraction = delta / containerWidth
      const current = pendingWidthsRef.current ?? [...widths]
      const newWidths = [...current]
      const left = (newWidths[leftIndex] ?? 0) + fraction
      const right = (newWidths[leftIndex + 1] ?? 0) - fraction
      if (left < 0.1 || right < 0.1) return
      newWidths[leftIndex] = left
      newWidths[leftIndex + 1] = right
      pendingWidthsRef.current = newWidths
      const leftEl = groupElsRef.current[leftIndex]
      const rightEl = groupElsRef.current[leftIndex + 1]
      if (leftEl) leftEl.style.flex = `${left} 0 0`
      if (rightEl) rightEl.style.flex = `${right} 0 0`
    },
    [widths, groups],
  )

  const handleColumnResizeEnd = useCallback(() => {
    const pending = pendingWidthsRef.current
    if (pending !== null && pending.length === widths.length) {
      void setGroupWidths(pending)
    }
    pendingWidthsRef.current = null
  }, [widths.length, setGroupWidths])

  const handleResetDivider = useCallback(() => {
    pendingWidthsRef.current = null
    setGroupWidths(groups.map(() => 1 / groups.length))
  }, [groups, setGroupWidths])

  return {
    groups,
    focusedGroupIndex,
    containerRef,
    registerGroup,
    widths,
    focusGroup,
    openInGroup,
    closeTab,
    closeAllTabs,
    closeOtherTabs,
    closeGroup,
    moveTabToGroup,
    handleColumnDrag,
    handleColumnResizeEnd,
    handleResetDivider,
  }
}
