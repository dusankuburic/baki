import {useCallback, useEffect, useRef, useState} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'

const MIN_SIDEBAR = 200
const MAX_SIDEBAR = 480
const MIN_INSPECTOR = 280
const MAX_INSPECTOR = 560

export function usePaneResize() {
  const layout = useSettingsStore(s => s.settings.layout)
  const updateLayout = useSettingsStore(s => s.updateLayout)

  const [sidebarLiveWidth, setSidebarLiveWidth] = useState<number | null>(null)
  const [inspectorLiveWidth, setInspectorLiveWidth] = useState<number | null>(null)
  const sidebarLiveWidthRef = useRef<number | null>(null)
  const inspectorLiveWidthRef = useRef<number | null>(null)
  const layoutRef = useRef(layout)
  useEffect(() => {
    layoutRef.current = layout
  })

  const handleSidebarDrag = useCallback((delta: number) => {
    const base = sidebarLiveWidthRef.current ?? layoutRef.current.sidebarWidth
    const next = Math.round(Math.min(MAX_SIDEBAR, Math.max(MIN_SIDEBAR, base + delta)))
    sidebarLiveWidthRef.current = next
    setSidebarLiveWidth(next)
  }, [])

  const handleSidebarResizeEnd = useCallback(() => {
    if (sidebarLiveWidthRef.current !== null) {
      updateLayout({sidebarWidth: sidebarLiveWidthRef.current})
      sidebarLiveWidthRef.current = null
      setSidebarLiveWidth(null)
    }
  }, [updateLayout])

  const handleInspectorDrag = useCallback((delta: number) => {
    const base = inspectorLiveWidthRef.current ?? layoutRef.current.inspectorWidth
    const next = Math.round(Math.min(MAX_INSPECTOR, Math.max(MIN_INSPECTOR, base - delta)))
    inspectorLiveWidthRef.current = next
    setInspectorLiveWidth(next)
  }, [])

  const handleInspectorResizeEnd = useCallback(() => {
    if (inspectorLiveWidthRef.current !== null) {
      updateLayout({inspectorWidth: inspectorLiveWidthRef.current})
      inspectorLiveWidthRef.current = null
      setInspectorLiveWidth(null)
    }
  }, [updateLayout])

  const handleSidebarReset = useCallback(() => updateLayout({sidebarWidth: 280}), [updateLayout])
  const handleInspectorReset = useCallback(() => updateLayout({inspectorWidth: 320}), [updateLayout])

  return {
    sidebarWidth: sidebarLiveWidth ?? layout.sidebarWidth,
    inspectorWidth: inspectorLiveWidth ?? layout.inspectorWidth,
    handleSidebarDrag,
    handleSidebarResizeEnd,
    handleSidebarReset,
    handleInspectorDrag,
    handleInspectorResizeEnd,
    handleInspectorReset,
  }
}
