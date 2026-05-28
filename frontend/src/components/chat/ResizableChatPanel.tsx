import {useRef, useState, useCallback, useEffect, ReactNode} from 'react'
import {GripVertical, Maximize2, Minimize2} from 'lucide-react'
import {useSettingsStore} from '@/stores/settingsStore'

interface Props {
  children: ReactNode
  minHeight?: number
  maxHeight?: number
}

const MIN_HEIGHT = 300
const MAX_HEIGHT = 900

export default function ResizableChatPanel({
  children,
  minHeight = MIN_HEIGHT,
  maxHeight = MAX_HEIGHT,
}: Props) {
  const containerRef = useRef<HTMLDivElement>(null)
  const resizeHandleRef = useRef<HTMLDivElement>(null)
  const [height, setHeight] = useState<number | null>(null) // null = auto (fill space)
  const [isResizing, setIsResizing] = useState(false)
  const [isPoppedOut, setIsPoppedOut] = useState(false)

  // Don't load saved height - start with auto-fill (null)
  // User can resize to set a specific height, which will be saved for future sessions

  const startResize = useCallback((e: React.MouseEvent) => {
    e.preventDefault()
    setIsResizing(true)
  }, [])

  const stopResize = useCallback(() => {
    setIsResizing(false)
  }, [])

  const resize = useCallback((e: MouseEvent) => {
    if (!isResizing || !containerRef.current) return

    const containerRect = containerRef.current.getBoundingClientRect()
    const newHeight = containerRect.bottom - e.clientY

    const clampedHeight = Math.max(minHeight, Math.min(maxHeight, newHeight))
    setHeight(clampedHeight)

    // Save to settings
    useSettingsStore.getState().updateSettings({
      layout: {
        ...useSettingsStore.getState().settings.layout,
        chatPanelHeight: clampedHeight,
      },
    })
  }, [isResizing, minHeight, maxHeight])

  // Double-click to reset to auto-fill
  const handleDoubleClick = useCallback(() => {
    setHeight(null)
    useSettingsStore.getState().updateSettings({
      layout: {
        ...useSettingsStore.getState().settings.layout,
        chatPanelHeight: undefined,
      },
    })
  }, [])

  useEffect(() => {
    if (isResizing) {
      document.addEventListener('mousemove', resize)
      document.addEventListener('mouseup', stopResize)
      return () => {
        document.removeEventListener('mousemove', resize)
        document.removeEventListener('mouseup', stopResize)
      }
    }
  }, [isResizing, resize, stopResize])

  const togglePopOut = useCallback(() => {
    setIsPoppedOut(prev => !prev)
  }, [])

  if (isPoppedOut) {
    return (
      <div className="fixed inset-4 z-50 bg-surface-0 border border-border-subtle rounded-xl shadow-2xl flex flex-col animate-fade-in">
        <div className="flex items-center justify-between px-3 py-2 border-b border-border-subtle">
          <span className="text-xs font-medium text-text-secondary">AI Chat</span>
          <button
            className="p-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-text-secondary transition-colors"
            onClick={togglePopOut}
            title="Dock to panel"
          >
            <Minimize2 size={14} />
          </button>
        </div>
        <div className="flex-1 overflow-hidden">
          {children}
        </div>
      </div>
    )
  }

  return (
    <div
      ref={containerRef}
      className="relative flex flex-col group h-full"
      style={height ? {height: `${height}px`, transition: isResizing ? 'none' : 'height 150ms var(--ease-out)'} : {transition: isResizing ? 'none' : 'height 150ms var(--ease-out)'}}
    >
      {/* Pop-out button */}
      <button
        className="absolute top-2 right-2 z-10 p-1.5 rounded-lg bg-surface-2/80 backdrop-blur border border-border-subtle hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-all opacity-0 group-hover:opacity-100"
        onClick={togglePopOut}
        title="Pop out to floating panel"
      >
        <Maximize2 size={12} />
      </button>

      {/* Scrollable content area */}
      <div className="flex-1 overflow-y-auto overflow-x-hidden">
        {children}
      </div>

      {/* Resize handle at the bottom */}
      <div
        ref={resizeHandleRef}
        className={clsx(
          'absolute bottom-0 left-0 right-0 h-4 cursor-ns-resize flex items-center justify-center',
          'hover:bg-brand-500/10 active:bg-brand-500/20 transition-colors',
          isResizing && 'bg-brand-500/20'
        )}
        onMouseDown={startResize}
        onDoubleClick={handleDoubleClick}
        title="Drag to resize · Double-click to reset to auto-fill"
      >
        <GripVertical size={14} className="text-text-tertiary/50 group-hover:text-brand-500/70 transition-colors" />
      </div>
    </div>
  )
}

// Helper to avoid import issues
function clsx(...classes: (string | boolean | undefined | null)[]) {
  return classes.filter(Boolean).join(' ')
}
