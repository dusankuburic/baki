import {useCallback, useEffect, useRef} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'

const MIN_SIDEBAR = 200
const MAX_SIDEBAR = 480
const MIN_INSPECTOR = 280
const MAX_INSPECTOR = 560

/**
 * Pane resizing without re-render storms.
 *
 * During a drag the width is written DIRECTLY to the pane element's style
 * (mutate `sidebarRef.current.style.width`) — no React state updates per
 * pointermove, so dragging never re-renders the app shell. The store's
 * layout is committed ONCE in the resize-end handler, and that commit is
 * what the rendered `style={{width}}` follows between drags.
 *
 * Attach the returned refs to the sidebar/inspector container elements.
 */
export function usePaneResize() {
  const layout = useSettingsStore(s => s.settings.layout)
  const updateLayout = useSettingsStore(s => s.updateLayout)

  const sidebarRef = useRef<HTMLDivElement | null>(null)
  const inspectorRef = useRef<HTMLDivElement | null>(null)

  const sidebarPendingRef = useRef<number | null>(null)
  const inspectorPendingRef = useRef<number | null>(null)

  // Keep the DOM in sync with the committed layout when it changes outside a
  // drag (settings load, reset, another window's edit). During a drag these
  // writes are skipped — the pending value owns the element until commit.
  useEffect(() => {
    if (sidebarPendingRef.current === null && sidebarRef.current) {
      sidebarRef.current.style.width = layout.sidebarWidth + 'px'
    }
  }, [layout.sidebarWidth])
  useEffect(() => {
    if (inspectorPendingRef.current === null && inspectorRef.current) {
      inspectorRef.current.style.width = layout.inspectorWidth + 'px'
    }
  }, [layout.inspectorWidth])

  const handleSidebarDrag = useCallback(
    (delta: number) => {
      const base = sidebarPendingRef.current ?? (parseInt(sidebarRef.current?.style.width || '') || layout.sidebarWidth)
      const next = Math.round(Math.min(MAX_SIDEBAR, Math.max(MIN_SIDEBAR, base + delta)))
      sidebarPendingRef.current = next
      if (sidebarRef.current) {
        sidebarRef.current.style.width = next + 'px'
      }
    },
    [layout.sidebarWidth],
  )

  const handleSidebarResizeEnd = useCallback(() => {
    if (sidebarPendingRef.current !== null) {
      void updateLayout({sidebarWidth: sidebarPendingRef.current})
      sidebarPendingRef.current = null
    }
  }, [updateLayout])

  const handleInspectorDrag = useCallback(
    (delta: number) => {
      const base =
        inspectorPendingRef.current ?? (parseInt(inspectorRef.current?.style.width || '') || layout.inspectorWidth)
      const next = Math.round(Math.min(MAX_INSPECTOR, Math.max(MIN_INSPECTOR, base - delta)))
      inspectorPendingRef.current = next
      if (inspectorRef.current) {
        inspectorRef.current.style.width = next + 'px'
      }
    },
    [layout.inspectorWidth],
  )

  const handleInspectorResizeEnd = useCallback(() => {
    if (inspectorPendingRef.current !== null) {
      void updateLayout({inspectorWidth: inspectorPendingRef.current})
      inspectorPendingRef.current = null
    }
  }, [updateLayout])

  const handleSidebarReset = useCallback(() => updateLayout({sidebarWidth: 280}), [updateLayout])
  const handleInspectorReset = useCallback(() => updateLayout({inspectorWidth: 320}), [updateLayout])

  return {
    sidebarRef,
    inspectorRef,
    sidebarWidth: layout.sidebarWidth,
    inspectorWidth: layout.inspectorWidth,
    handleSidebarDrag,
    handleSidebarResizeEnd,
    handleSidebarReset,
    handleInspectorDrag,
    handleInspectorResizeEnd,
    handleInspectorReset,
  }
}
