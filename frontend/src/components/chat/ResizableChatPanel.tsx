import {useTranslation} from 'react-i18next'
import {createContext, useContext, useMemo, useRef, useState, useCallback, useEffect, ReactNode} from 'react'
import {Minimize2} from 'lucide-react'
import {useSettingsStore} from '@/stores/settingsStore'
import clsx from 'clsx'

// The pop-out toggle lives in the chat's overflow menu rather than on a
// hover-only floating button that sat on top of the header. AITab reaches it
// through this context; it is nullable so AITab still renders standalone (in
// tests, and anywhere the panel wrapper isn't used).
interface ChatPanelApi {
  isPoppedOut: boolean
  togglePopOut: () => void
}
const ChatPanelContext = createContext<ChatPanelApi | null>(null)
export function useChatPanel(): ChatPanelApi | null {
  return useContext(ChatPanelContext)
}

interface Props {
  children: ReactNode
  minHeight?: number
  maxHeight?: number
}

const MIN_HEIGHT = 300
const MAX_HEIGHT = 900

export default function ResizableChatPanel({children, minHeight = MIN_HEIGHT, maxHeight = MAX_HEIGHT}: Props) {
  const {t} = useTranslation('chat')
  const containerRef = useRef<HTMLDivElement>(null)
  const startYRef = useRef(0)
  const startHeightRef = useRef(0)
  const pendingHeightRef = useRef<number | null>(null)
  const isResizingRef = useRef(false)
  const [height, setHeight] = useState<number | null>(null)
  const [isResizing, setIsResizing] = useState(false)
  const [isPoppedOut, setIsPoppedOut] = useState(false)

  const startResize = useCallback((e: React.MouseEvent) => {
    e.preventDefault()
    startYRef.current = e.clientY
    startHeightRef.current = containerRef.current?.getBoundingClientRect().height ?? 0
    pendingHeightRef.current = null
    isResizingRef.current = true
    setIsResizing(true)
    document.body.style.cursor = 'ns-resize'
  }, [])

  // DOM mutation only — no React state update on each pixel.
  // setHeight is called once in stopResize to sync React state with the final value.
  const resize = useCallback(
    (e: MouseEvent) => {
      if (!isResizingRef.current) return
      const delta = startYRef.current - e.clientY
      // Round to whole pixels: clientY is fractional on HiDPI displays and the
      // persisted chatPanelHeight is an int server-side (decode rejects floats).
      const newHeight = Math.round(Math.max(minHeight, Math.min(maxHeight, startHeightRef.current + delta)))
      pendingHeightRef.current = newHeight
      if (containerRef.current) {
        containerRef.current.style.height = newHeight + 'px'
      }
    },
    [minHeight, maxHeight],
  )

  const stopResize = useCallback(() => {
    if (!isResizingRef.current) return
    isResizingRef.current = false
    setIsResizing(false)
    document.body.style.cursor = ''
    if (pendingHeightRef.current !== null) {
      const finalHeight = pendingHeightRef.current
      setHeight(finalHeight)
      void useSettingsStore.getState().updateSettings({
        layout: {
          ...useSettingsStore.getState().settings.layout,
          chatPanelHeight: finalHeight,
        },
      })
    }
  }, [])

  const handleDoubleClick = useCallback(() => {
    setHeight(null)
    if (containerRef.current) {
      containerRef.current.style.height = ''
    }
    void useSettingsStore.getState().updateSettings({
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

  const panelApi = useMemo<ChatPanelApi>(() => ({isPoppedOut, togglePopOut}), [isPoppedOut, togglePopOut])

  if (isPoppedOut) {
    return (
      <div className="fixed inset-4 z-modal bg-surface-0 border border-border-subtle rounded-xl shadow-2xl flex flex-col animate-fade-in">
        <div className="flex items-center justify-between px-3 py-2 border-b border-border-subtle">
          <span className="text-xs font-medium text-text-secondary">{t('panel.title')}</span>
          <button
            className="p-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-text-secondary transition-colors"
            onClick={togglePopOut}
            title={t('panel.dock')}
            aria-label={t('panel.dock')}
          >
            <Minimize2 size={14} />
          </button>
        </div>
        <div className="flex-1 min-h-0 overflow-hidden">
          <ChatPanelContext.Provider value={panelApi}>{children}</ChatPanelContext.Provider>
        </div>
      </div>
    )
  }

  return (
    <div
      ref={containerRef}
      className={clsx('relative flex flex-col group h-full', isResizing && 'select-none')}
      style={{
        ...(height ? {height: `${height}px`} : {}),
        transition: isResizing ? 'none' : 'height 150ms var(--ease-out)',
        willChange: isResizing ? 'height' : 'auto',
      }}
    >
      <div className="flex-1 min-h-0 overflow-hidden">
        <ChatPanelContext.Provider value={panelApi}>{children}</ChatPanelContext.Provider>
      </div>

      {/* Resize handle — three-dot pill indicator with accent line */}
      <div
        className={clsx(
          'absolute bottom-0 left-0 right-0 h-2.5 cursor-ns-resize flex items-center justify-center group/handle',
          isResizing ? 'bg-brand-500/10' : 'hover:bg-brand-500/5',
        )}
        onMouseDown={startResize}
        onDoubleClick={handleDoubleClick}
        title={t('panel.resizeHint')}
      >
        <div
          className={clsx(
            'flex items-center gap-[3px] transition-all duration-150',
            isResizing ? 'opacity-100 scale-110' : 'opacity-0 group-hover/handle:opacity-50',
          )}
        >
          <div className="w-[3px] h-[3px] rounded-full bg-brand-400" />
          <div className="w-[3px] h-[3px] rounded-full bg-brand-400" />
          <div className="w-[3px] h-[3px] rounded-full bg-brand-400" />
        </div>
        {/* Accent line */}
        <div
          className={clsx(
            'absolute bottom-0 left-6 right-6 h-px bg-brand-500 transition-all duration-200 origin-center',
            isResizing
              ? 'opacity-70 scale-x-100'
              : 'opacity-0 scale-x-0 group-hover/handle:opacity-25 group-hover/handle:scale-x-100',
          )}
        />
      </div>
    </div>
  )
}
