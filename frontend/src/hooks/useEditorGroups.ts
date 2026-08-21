import {useMemo, useRef, useCallback} from 'react'
import {useEditorStore} from '@/stores/editorStore'

// useEditorGroups owns the split-pane editor-group state and the drag/resize
// math for the column dividers between groups. Extracted from MainPane so the
// view component is just routing + rendering: the editor-store hooks, the
// width memo, and the drag/reset handlers live here and are independently
// testable.
//
// containerRef must be attached to the flex row that contains the groups so
// handleColumnDrag can translate pixel deltas into fractions of the row width.
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
  // DOM elements of the group panes (registered by MainPane) + the pending
  // in-drag fractions. During a divider drag the two adjacent groups' style.flex
  // is mutated directly — no store update per pointermove, so the editor row,
  // tab strips, and virtualized block views don't re-render at drag frequency.
  // The store's groupWidths is committed once on the divider's resize end.
  const groupElsRef = useRef<Array<HTMLDivElement | null>>([])
  const pendingWidthsRef = useRef<number[] | null>(null)

  const registerGroup = useCallback((gi: number) => (el: HTMLDivElement | null) => {
    groupElsRef.current[gi] = el
  }, [])

  // Effective per-group widths (fractions). Falls back to stored widths when
  // they match the current group count, else an even split.
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
      const left = newWidths[leftIndex] + fraction
      const right = newWidths[leftIndex + 1] - fraction
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

  // Commit the drag's final fractions to the store (single update per gesture).
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
