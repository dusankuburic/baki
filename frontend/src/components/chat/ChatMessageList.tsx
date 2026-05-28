import {useRef, useEffect, useCallback, useState, memo} from 'react'
import {ChevronDown} from 'lucide-react'
import clsx from 'clsx'

interface Props {
  children: React.ReactNode
  isStreaming?: boolean
}

const SCROLL_UPDATE_THROTTLE = 120 // ms between scroll updates
const SCROLL_THRESHOLD = 100 // pixels from bottom to trigger auto-scroll

function ChatMessageList({children, isStreaming}: Props) {
  const containerRef = useRef<HTMLDivElement>(null)
  const [showScrollBtn, setShowScrollBtn] = useState(false)
  const userScrolledRef = useRef(false)

  const handleScroll = useCallback(() => {
    const el = containerRef.current
    if (!el) return
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60
    setShowScrollBtn(!nearBottom)
    if (!nearBottom) {
      userScrolledRef.current = true
    }
  }, [])

  const scrollToBottom = useCallback((smooth = true) => {
    const el = containerRef.current
    if (!el) return
    el.scrollTo({
      top: el.scrollHeight,
      behavior: smooth ? 'smooth' : 'instant',
    })
    userScrolledRef.current = false
    setShowScrollBtn(false)
  }, [])

  useEffect(() => {
    if (isStreaming) {
      userScrolledRef.current = false
    }
  }, [isStreaming])

  // Throttled smooth scroll during streaming
  useEffect(() => {
    if (!isStreaming || userScrolledRef.current) return

    const interval = setInterval(() => {
      const el = containerRef.current
      if (!el) return

      const distanceToBottom = el.scrollHeight - el.scrollTop - el.clientHeight
      if (distanceToBottom < SCROLL_THRESHOLD) {
        el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' })
      }
    }, SCROLL_UPDATE_THROTTLE)

    return () => clearInterval(interval)
  }, [isStreaming])

  useEffect(() => {
    if (isStreaming && !userScrolledRef.current) {
      const el = containerRef.current
      if (el) {
        el.scrollTop = el.scrollHeight
      }
    }
  }, [isStreaming, children])

  useEffect(() => {
    const el = containerRef.current
    if (!el) return
    if (!userScrolledRef.current) {
      scrollToBottom(false)
    }
  }, [children, scrollToBottom])

  return (
    <div className="flex-1 overflow-hidden relative">
      <div
        ref={containerRef}
        className={clsx(
          'h-full overflow-y-auto px-3 py-3 flex flex-col gap-3 custom-scrollbar',
          isStreaming && 'is-streaming'
        )}
        onScroll={handleScroll}
      >
        {children}
      </div>
      {showScrollBtn && (
        <button
          className="absolute bottom-2 left-1/2 -translate-x-1/2 p-1.5 rounded-full bg-surface-2 border border-border-default shadow-md text-text-secondary hover:text-text-primary hover:bg-surface-3 transition-all animate-fade-in z-10"
          onClick={() => scrollToBottom()}
          aria-label="Scroll to bottom"
        >
          <ChevronDown size={14} />
        </button>
      )}
    </div>
  )
}

// Memoize to prevent re-renders during streaming - only re-render when isStreaming changes
export default memo(ChatMessageList, (prevProps, nextProps) => {
  return prevProps.isStreaming === nextProps.isStreaming
})
