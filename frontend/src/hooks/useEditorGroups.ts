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
      const newWidths = [...widths]
      const left = newWidths[leftIndex] + fraction
      const right = newWidths[leftIndex + 1] - fraction
      if (left < 0.1 || right < 0.1) return
      newWidths[leftIndex] = left
      newWidths[leftIndex + 1] = right
      setGroupWidths(newWidths)
    },
    [widths, groups, setGroupWidths],
  )

  const handleResetDivider = useCallback(() => {
    setGroupWidths(groups.map(() => 1 / groups.length))
  }, [groups, setGroupWidths])

  return {
    groups,
    focusedGroupIndex,
    containerRef,
    widths,
    focusGroup,
    openInGroup,
    closeTab,
    closeAllTabs,
    closeOtherTabs,
    closeGroup,
    moveTabToGroup,
    handleColumnDrag,
    handleResetDivider,
  }
}
