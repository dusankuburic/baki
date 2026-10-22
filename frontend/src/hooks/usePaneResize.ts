import {useCallback, useEffect, useRef} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'

export const MIN_SIDEBAR = 200
export const MAX_SIDEBAR = 480
export const MIN_INSPECTOR = 280
const INSPECTOR_HARD_MAX = 1000
const INSPECTOR_LEGACY_MAX = 560
const MIN_MAIN = 480

export function inspectorMaxFor(viewportWidth: number, sidebarWidth: number): number {
  return Math.max(INSPECTOR_LEGACY_MAX, Math.min(INSPECTOR_HARD_MAX, viewportWidth - sidebarWidth - MIN_MAIN))
}

function viewportWidth(): number {
  return typeof window === 'undefined' ? Number.POSITIVE_INFINITY : window.innerWidth
}

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

  useEffect(() => {
    if (sidebarPendingRef.current === null && sidebarRef.current) {
      sidebarRef.current.style.width = layout.sidebarWidth + 'px'
    }
  }, [layout.sidebarWidth])
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

  const handleSidebarReset = useCallback(() => {
    void updateLayout({sidebarWidth: 280})
  }, [updateLayout])
  const handleInspectorReset = useCallback(() => {
    void updateLayout({inspectorWidth: 320})
  }, [updateLayout])

  return {
    sidebarRef,
    inspectorRef,
    sidebarWidth: layout.sidebarWidth,
    inspectorWidth: layout.inspectorWidth,
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
