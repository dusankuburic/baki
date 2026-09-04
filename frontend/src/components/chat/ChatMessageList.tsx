import {useTranslation} from 'react-i18next'
import React, {useRef, useEffect, useState, useMemo, useCallback, memo} from 'react'
import {Virtuoso, type VirtuosoHandle} from 'react-virtuoso'
import {ChevronDown} from 'lucide-react'
import type {ChatMessage} from '@/types'

interface Props {
  messages: ChatMessage[]
  renderMessage: (index: number, message: ChatMessage) => React.ReactNode
  // Rendered after the last message (thinking indicator, streaming bubble,
  // live tool trail, pending fix cards). It grows without appending data
  // items, so a ResizeObserver — not followOutput — keeps it in view.
  footer?: React.ReactNode
  isStreaming?: boolean
  // Search navigation: scroll this message index into view. `nonce` changes on
  // every request so stepping onto the SAME index twice still scrolls (Virtuoso
  // has no imperative "re-run" and a bare index would compare equal).
  scrollTo?: {index: number; nonce: number}
}

// Everything per-render travels through Virtuoso's `context` prop. The
// `components` object below MUST keep a stable identity: rebuilding it (as a
// useMemo over `footer` used to) hands React a brand-new Scroller component
// TYPE on every parent render, which unmounts and remounts the whole scroll
// container — dropping scroll position on every search keystroke, thinking-
// state flip, fix proposal, and canvas selection.
interface ListContext {
  footer?: React.ReactNode
  footerRef: React.MutableRefObject<HTMLDivElement | null>
  label: string
}

// The Scroller carries the conversation's accessibility semantics and the item
// spacing that a flex gap used to provide: vertical rhythm comes from
// item/footer padding.
const Scroller = React.forwardRef<HTMLDivElement, {context?: ListContext} & React.HTMLAttributes<HTMLDivElement>>(
  function Scroller({style, context, ...props}, ref) {
    return (
      <div
        ref={ref}
        style={style}
        {...props}
        className="px-3 pt-3 custom-scrollbar overflow-y-auto"
        role="log"
        aria-live="polite"
        aria-relevant="additions text"
        aria-label={context?.label}
      />
    )
  },
)

function Footer({context}: {context?: ListContext}) {
  return (
    <div ref={context?.footerRef} className="chat-msg-item pb-3">
      {context?.footer}
    </div>
  )
}

const COMPONENTS = {Scroller, Footer}

// Virtualized conversation list (react-virtuoso — same library as the
// findings/block lists): only the visible window mounts (each MessageBubble
// carries react-markdown + Prism, so mounting all is O(messages)).
// Scroll semantics:
//   - opens at the bottom (initialTopMostItemIndex),
//   - follows new messages when already at the bottom (followOutput),
//   - during streaming, a ResizeObserver on the footer keeps the growing
//     bubble pinned while the user hasn't scrolled away,
//   - floating scroll-to-bottom button when detached from the bottom.
function ChatMessageList({messages, renderMessage, footer, isStreaming, scrollTo}: Props) {
  const {t} = useTranslation('chat')
  const virtuosoRef = useRef<VirtuosoHandle>(null)
  const footerRef = useRef<HTMLDivElement | null>(null)
  const [showScrollBtn, setShowScrollBtn] = useState(false)
  const atBottomRef = useRef(true)

  const scrollToBottom = useCallback((smooth = true) => {
    // 'auto' jumps directly; 'smooth' animates (Virtuoso has no 'instant').
    virtuosoRef.current?.scrollToIndex({index: 'LAST', behavior: smooth ? 'smooth' : 'auto'})
  }, [])

  // Reset bottom tracking and jump when a new stream starts — the user just
  // sent a message, so the bottom is where they want to be.
  useEffect(() => {
    if (isStreaming) {
      atBottomRef.current = true
      scrollToBottom(false)
    }
  }, [isStreaming, scrollToBottom])

  // The streaming footer grows without appending data items, so followOutput
  // alone never fires for it. One INSTANT scroll per observed size change
  // replaces the old 120ms interval, which ran a second smooth-scroll
  // animation against Virtuoso's own and made the list stutter.
  useEffect(() => {
    if (!isStreaming) return
    const el = footerRef.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(() => {
      if (atBottomRef.current) scrollToBottom(false)
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [isStreaming, scrollToBottom])

  // Centre the requested match so its surrounding turns are visible — the
  // same affordance FindingsList uses when deep-linking to a finding.
  useEffect(() => {
    if (!scrollTo) return
    virtuosoRef.current?.scrollToIndex({index: scrollTo.index, align: 'center', behavior: 'smooth'})
  }, [scrollTo])

  const context = useMemo<ListContext>(
    () => ({footer, footerRef, label: t('a11y.conversation')}),
    [footer, t],
  )

  return (
    <div className="flex-1 overflow-hidden relative">
      <Virtuoso
        ref={virtuosoRef}
        data={messages}
        context={context}
        itemContent={(i, m) => <div className="chat-msg-item pb-3">{renderMessage(i, m)}</div>}
        components={COMPONENTS}
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
          aria-label={t('a11y.scrollToBottom')}
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
