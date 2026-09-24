import {useCallback, useEffect, useRef} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'

export const MIN_SIDEBAR = 200
export const MAX_SIDEBAR = 480
export const MIN_INSPECTOR = 280
// The inspector never grows past INSPECTOR_HARD_MAX and always leaves the
// editor at least MIN_MAIN; on a narrow window the old 560 cap still applies,
// so nothing gets tighter than it used to be.
const INSPECTOR_HARD_MAX = 1000
const INSPECTOR_LEGACY_MAX = 560
const MIN_MAIN = 480

// inspectorMaxFor is the widest the inspector may be for a given viewport and
// sidebar width. Exported for the ARIA range on the divider and for tests.
export function inspectorMaxFor(viewportWidth: number, sidebarWidth: number): number {
  return Math.max(INSPECTOR_LEGACY_MAX, Math.min(INSPECTOR_HARD_MAX, viewportWidth - sidebarWidth - MIN_MAIN))
}

function viewportWidth(): number {
  return typeof window === 'undefined' ? Number.POSITIVE_INFINITY : window.innerWidth
}

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
  const sidebarCollapsed = useUIStore(s => s.sidebarCollapsed)

  const sidebarRef = useRef<HTMLDivElement | null>(null)
  const inspectorRef = useRef<HTMLDivElement | null>(null)

  const sidebarPendingRef = useRef<number | null>(null)
  const inspectorPendingRef = useRef<number | null>(null)

  const currentSidebarWidth = useCallback(() => {
    if (sidebarCollapsed) return 0
    return sidebarPendingRef.current ?? (parseInt(sidebarRef.current?.style.width || '') || layout.sidebarWidth)
  }, [sidebarCollapsed, layout.sidebarWidth])

  const inspectorMaxNow = useCallback(
    () => inspectorMaxFor(viewportWidth(), currentSidebarWidth()),
    [currentSidebarWidth],
  )

  // Keep the DOM in sync with the committed layout when it changes outside a
  // drag (settings load, reset, another window's edit). During a drag these
  // writes are skipped — the pending value owns the element until commit.
  useEffect(() => {
    if (sidebarPendingRef.current === null && sidebarRef.current) {
      sidebarRef.current.style.width = layout.sidebarWidth + 'px'
    }
  }, [layout.sidebarWidth])
  // The inspector is also clamped to what the window can hold: a width saved
  // on a wide monitor must not squeeze the editor on a narrow one. The stored
  // preference is left alone; only the element is clamped, and re-clamped on
  // every window resize.
  useEffect(() => {
    const apply = () => {
      if (inspectorPendingRef.current === null && inspectorRef.current) {
        inspectorRef.current.style.width = Math.min(layout.inspectorWidth, inspectorMaxNow()) + 'px'
      }
    }
    apply()
    if (typeof window === 'undefined') return
    window.addEventListener('resize', apply)
    return () => window.removeEventListener('resize', apply)
  }, [layout.inspectorWidth, inspectorMaxNow])

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
      // The divider sits on the inspector's LEFT edge: moving it left widens.
      const next = Math.round(Math.min(inspectorMaxNow(), Math.max(MIN_INSPECTOR, base - delta)))
      inspectorPendingRef.current = next
      if (inspectorRef.current) {
        inspectorRef.current.style.width = next + 'px'
      }
    },
    [layout.inspectorWidth, inspectorMaxNow],
  )

  const handleInspectorResizeEnd = useCallback(() => {
    if (inspectorPendingRef.current !== null) {
      void updateLayout({inspectorWidth: inspectorPendingRef.current})
      inspectorPendingRef.current = null
    }
  }, [updateLayout])

  // Home / End on the divider: an infinite delta lands on the clamp, and the
  // usual resize-end commits it once.
  const handleSidebarJump = useCallback(
    (to: 'min' | 'max') => {
      handleSidebarDrag(to === 'min' ? Number.NEGATIVE_INFINITY : Number.POSITIVE_INFINITY)
      handleSidebarResizeEnd()
    },
    [handleSidebarDrag, handleSidebarResizeEnd],
  )
  const handleInspectorJump = useCallback(
    (to: 'min' | 'max') => {
      handleInspectorDrag(to === 'min' ? Number.POSITIVE_INFINITY : Number.NEGATIVE_INFINITY)
      handleInspectorResizeEnd()
    },
    [handleInspectorDrag, handleInspectorResizeEnd],
  )

  const handleSidebarReset = useCallback(() => updateLayout({sidebarWidth: 280}), [updateLayout])
  const handleInspectorReset = useCallback(() => updateLayout({inspectorWidth: 320}), [updateLayout])

  return {
    sidebarRef,
    inspectorRef,
    sidebarWidth: layout.sidebarWidth,
    inspectorWidth: layout.inspectorWidth,
    // For the divider's aria-valuemax: from committed values only (no refs
    // during render), so it lags a sidebar drag until that drag commits.
    inspectorMax: inspectorMaxFor(viewportWidth(), sidebarCollapsed ? 0 : layout.sidebarWidth),
    handleSidebarDrag,
    handleSidebarResizeEnd,
    handleSidebarReset,
    handleSidebarJump,
    handleInspectorDrag,
    handleInspectorResizeEnd,
    handleInspectorReset,
    handleInspectorJump,
  }
}
