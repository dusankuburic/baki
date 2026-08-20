import React, {useRef, useEffect, useState, useMemo, memo} from 'react'
import {Virtuoso, type VirtuosoHandle} from 'react-virtuoso'
import {ChevronDown} from 'lucide-react'
import clsx from 'clsx'
import type {ChatMessage} from '@/types'

interface Props {
  messages: ChatMessage[]
  renderMessage: (index: number, message: ChatMessage) => React.ReactNode
  // Rendered after the last message (thinking indicator, streaming bubble).
  // Its content growing does NOT auto-scroll by itself — the streaming
  // interval below handles that, mirroring the pre-virtualized behavior.
  footer?: React.ReactNode
  isStreaming?: boolean
}

const SCROLL_UPDATE_THROTTLE = 120 // ms between scroll updates

// Virtualized conversation list (react-virtuoso — same library as the
// findings/block lists). Long threads previously mounted every MessageBubble
// (react-markdown + Prism per message), making thread open/switch O(messages);
// now only the visible window mounts. Scroll semantics are preserved:
//   - opens at the bottom (initialTopMostItemIndex),
//   - follows new messages when already at the bottom (followOutput),
//   - during streaming, interval-smooth-scrolls while the user hasn't
//     scrolled away (atBottomStateChange tracks that),
//   - floating scroll-to-bottom button when detached from the bottom.
function ChatMessageList({messages, renderMessage, footer, isStreaming}: Props) {
  const virtuosoRef = useRef<VirtuosoHandle>(null)
  const scrollerRef = useRef<HTMLDivElement | null>(null)
  const [showScrollBtn, setShowScrollBtn] = useState(false)
  const atBottomRef = useRef(true)

  const scrollToBottom = (smooth = true) => {
    // 'auto' jumps directly; 'smooth' animates (Virtuoso has no 'instant').
    virtuosoRef.current?.scrollToIndex({index: 'LAST', behavior: smooth ? 'smooth' : 'auto'})
  }

  // Reset bottom tracking and jump when a new stream starts.
  useEffect(() => {
    if (isStreaming) {
      atBottomRef.current = true
      scrollToBottom(false)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isStreaming])

  // Smooth auto-scroll during streaming: the streaming footer grows without
  // appending data items, so followOutput alone wouldn't fire — poll and
  // scroll unless the user scrolled away.
  useEffect(() => {
    if (!isStreaming) return
    const interval = setInterval(() => {
      if (!atBottomRef.current) return
      const el = scrollerRef.current
      if (!el) return
      const distanceToBottom = el.scrollHeight - el.scrollTop - el.clientHeight
      if (distanceToBottom > 2) {
        el.scrollTo({top: el.scrollHeight, behavior: 'smooth'})
      }
    }, SCROLL_UPDATE_THROTTLE)
    return () => clearInterval(interval)
  }, [isStreaming])

  // The Scroller carries the conversation's accessibility semantics (kept
  // from the pre-virtualized div) and the item spacing that a flex gap used
  // to provide: vertical rhythm comes from item/footer padding.
  const components = useMemo(
    () => ({
      Scroller: React.forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(function Scroller(
        {style, ...props},
        ref,
      ) {
        return (
          <div
            ref={ref}
            style={style}
            {...props}
            className={clsx(
              'px-3 pt-3 custom-scrollbar overflow-y-auto',
              isStreaming && 'is-streaming',
            )}
            role="log"
            aria-live="polite"
            aria-relevant="additions text"
            aria-label="Conversation"
          />
        )
      }),
      Footer:
        footer !== undefined
          ? function Footer() {
              return <div className="pb-3">{footer}</div>
            }
          : undefined,
    }),
    [footer, isStreaming],
  )

  return (
    <div className="flex-1 overflow-hidden relative">
      <Virtuoso
        ref={virtuosoRef}
        scrollerRef={el => {
          scrollerRef.current = el as HTMLDivElement | null
        }}
        data={messages}
        itemContent={(i, m) => <div className="pb-3">{renderMessage(i, m)}</div>}
        components={components}
        initialTopMostItemIndex={Math.max(0, messages.length - 1)}
        followOutput={isStreaming ? 'smooth' : 'auto'}
        atBottomStateChange={atBottom => {
          atBottomRef.current = atBottom
          setShowScrollBtn(!atBottom)
        }}
        increaseViewportBy={{top: 400, bottom: 400}}
        style={{height: '100%'}}
      />
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
// comparison so the messages array / footer (live streaming bubble) always update.
export default memo(ChatMessageList)
