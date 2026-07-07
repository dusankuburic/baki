import {useRef, useEffect, useCallback, useState, memo} from 'react'
import {ChevronDown} from 'lucide-react'
import clsx from 'clsx'

interface Props {
  children: React.ReactNode
  isStreaming?: boolean
}

const SCROLL_UPDATE_THROTTLE = 120 // ms between scroll updates

function ChatMessageList({children, isStreaming}: Props) {
  const containerRef = useRef<HTMLDivElement>(null)
  const [showScrollBtn, setShowScrollBtn] = useState(false)
  const userScrolledRef = useRef(false)

  const handleScroll = useCallback(() => {
    const el = containerRef.current
    if (!el) return
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60
    setShowScrollBtn(!nearBottom)
    // Reset tracking when user scrolls back to bottom so auto-scroll resumes
    userScrolledRef.current = !nearBottom
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

  // Reset user-scroll tracking and jump to bottom when a new stream starts
  useEffect(() => {
    if (isStreaming) {
      userScrolledRef.current = false
      const el = containerRef.current
      if (el) el.scrollTop = el.scrollHeight
    }
  }, [isStreaming])

  // Smooth auto-scroll during streaming (interval-based, respects user scroll)
  useEffect(() => {
    if (!isStreaming) return
    const interval = setInterval(() => {
      if (userScrolledRef.current) return
      const el = containerRef.current
      if (!el) return
      const distanceToBottom = el.scrollHeight - el.scrollTop - el.clientHeight
      if (distanceToBottom > 2) {
        el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' })
      }
    }, SCROLL_UPDATE_THROTTLE)
    return () => clearInterval(interval)
  }, [isStreaming])

  // Instant scroll to bottom when non-streaming messages are added
  useEffect(() => {
    if (!isStreaming && !userScrolledRef.current) {
      const el = containerRef.current
      if (el) el.scrollTo({ top: el.scrollHeight, behavior: 'instant' })
    }
  }, [children, isStreaming])

  return (
    <div className="flex-1 overflow-hidden relative">
      <div
        ref={containerRef}
        className={clsx(
          'h-full overflow-y-auto px-3 py-3 flex flex-col gap-3 custom-scrollbar',
          isStreaming && 'is-streaming'
        )}
        onScroll={handleScroll}
        role="log"
        aria-live="polite"
        aria-relevant="additions text"
        aria-label="Conversation"
      >
        {children}
      </div>
      {showScrollBtn && (
        <button
          className="absolute bottom-2 left-1/2 -translate-x-1/2 p-1.5 rounded-full bg-surface-2 border border-border-default shadow-md text-text-secondary hover:text-text-primary hover:bg-surface-3 transition-all duration-fast animate-fade-in z-10"
          onClick={() => scrollToBottom()}
          aria-label="Scroll to bottom"
        >
          <ChevronDown size={14} />
        </button>
      )}
    </div>
  )
}

// Memoize to avoid unnecessary re-renders, but use React's default shallow
// comparison so children (including the live streaming bubble) always update.
export default memo(ChatMessageList)
