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
  onWheel: () => void
}

// The Scroller carries the conversation's accessibility semantics and the item
// spacing that a flex gap used to provide: vertical rhythm comes from
// item/footer padding. It has NO padding of its own: Virtuoso positions its
// viewport absolutely at 100% of the scroller's padding box, so 12px of
// side padding here pushed the whole list 12px past the edge and the
// conversation scrolled sideways at every width. The side gutter is the
// items' (.chat-msg-item) and the top inset is the Header's. overflow-x is
// hidden as the backstop: nothing in a message may widen the column.
const Scroller = React.forwardRef<HTMLDivElement, {context?: ListContext} & React.HTMLAttributes<HTMLDivElement>>(
  function Scroller({style, context, ...props}, ref) {
    return (
      <div
        ref={ref}
        style={style}
        {...props}
        onWheel={context?.onWheel}
        className="custom-scrollbar overflow-y-auto overflow-x-hidden"
        role="log"
        aria-live="polite"
        aria-relevant="additions text"
        aria-label={context?.label}
      />
    )
  },
)

function Header() {
  return <div className="h-3" />
}

function Footer({context}: {context?: ListContext}) {
  return (
    <div ref={context?.footerRef} className="chat-msg-item pb-3">
      {context?.footer}
    </div>
  )
}

const COMPONENTS = {Scroller, Header, Footer}

// How close to the end still counts as "at the bottom": enough to absorb
// sub-pixel rounding and a partly revealed last line, small enough that one
// wheel notch upwards releases the pin.
const AT_BOTTOM_PX = 24

function distanceFromBottom(el: HTMLElement): number {
  return el.scrollHeight - el.clientHeight - el.scrollTop
}

// Virtualized conversation list (react-virtuoso — same library as the
// findings/block lists): only the visible window mounts (each MessageBubble
// carries react-markdown + Prism, so mounting all is O(messages)).
// Scroll semantics:
//   - opens at the bottom (initialTopMostItemIndex),
//   - follows new messages when already at the bottom (followOutput),
//   - during streaming, a ResizeObserver on the footer keeps the growing
//     bubble pinned while the user hasn't scrolled away,
//   - floating scroll-to-bottom button when detached from the bottom.
//
// Pinning writes the scroller's own scrollTop = scrollHeight, and "at the
// bottom" is read back from the scroller on every scroll event. It used to go
// through Virtuoso's scrollToIndex('LAST'), which is a SEEK: it computes the
// target from its size cache (a chunk or two stale while a reply streams) and
// re-issues the scroll whenever the list changes in the next 150ms. During
// streaming the list changes constantly, so a retry could fire after the user
// had scrolled away and yank the view back, or land in the middle of a reply.
function ChatMessageList({messages, renderMessage, footer, isStreaming, scrollTo}: Props) {
  const {t} = useTranslation('chat')
  const virtuosoRef = useRef<VirtuosoHandle>(null)
  const containerRef = useRef<HTMLDivElement | null>(null)
  const scrollerRef = useRef<HTMLElement | null>(null)
  const footerRef = useRef<HTMLDivElement | null>(null)
  const [showScrollBtn, setShowScrollBtn] = useState(false)
  const atBottomRef = useRef(true)
  // Timestamp of the last wheel event, used to keep auto-pinning from
  // fighting an active user scroll (see the ResizeObserver below).
  const lastWheelRef = useRef(0)
  const handleWheel = useCallback(() => {
    lastWheelRef.current = Date.now()
  }, [])

  const syncAtBottom = useCallback(() => {
    const el = scrollerRef.current
    if (!el) return
    const atBottom = distanceFromBottom(el) <= AT_BOTTOM_PX
    atBottomRef.current = atBottom
    setShowScrollBtn(!atBottom)
  }, [])

  // Virtuoso hands over its scroll container; the at-bottom state is read from
  // it directly on every scroll, the user's and our own alike.
  const handleScrollerRef = useCallback(
    (ref: HTMLElement | Window | null) => {
      const el = ref instanceof HTMLElement ? ref : null
      scrollerRef.current?.removeEventListener('scroll', syncAtBottom)
      scrollerRef.current = el
      if (el) {
        el.addEventListener('scroll', syncAtBottom, {passive: true})
        syncAtBottom()
      }
    },
    [syncAtBottom],
  )
  useEffect(() => () => scrollerRef.current?.removeEventListener('scroll', syncAtBottom), [syncAtBottom])

  const scrollToBottom = useCallback((smooth = true) => {
    const el = scrollerRef.current
    if (!el) return
    const top = el.scrollHeight
    if (smooth && typeof el.scrollTo === 'function') el.scrollTo({top, behavior: 'smooth'})
    else el.scrollTop = top
  }, [])

  // Reset bottom tracking and jump when a new stream starts — the user just
  // sent a message, so the bottom is where they want to be. The footer mounts
  // with the stream; pin once it has laid out.
  useEffect(() => {
    if (!isStreaming) return
    atBottomRef.current = true
    const raf = requestAnimationFrame(() => scrollToBottom(false))
    return () => cancelAnimationFrame(raf)
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
      // While the user is actively wheeling, hold off pinning: an instant
      // scroll cancels the wheel's momentum. The next chunk re-pins once the
      // wheel is quiet — if they are still at the bottom.
      if (Date.now() - lastWheelRef.current < 400) return
      if (atBottomRef.current) scrollToBottom(false)
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [isStreaming, scrollToBottom])

  // The list's width jumps when the panel is dragged or focus mode toggles:
  // rows re-measure and the bottom drifts. If the user was at the bottom,
  // pin it again.
  useEffect(() => {
    const el = containerRef.current
    if (!el || typeof ResizeObserver === 'undefined') return
    let lastWidth = el.clientWidth
    const ro = new ResizeObserver(entries => {
      const width = entries[0]?.contentRect.width ?? el.clientWidth
      if (width === lastWidth) return
      lastWidth = width
      if (atBottomRef.current) scrollToBottom(false)
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [scrollToBottom])

  // Centre the requested match so its surrounding turns are visible — the
  // same affordance FindingsList uses when deep-linking to a finding. This is
  // a genuine seek into possibly unmeasured items, so Virtuoso's scrollToIndex
  // (with its retries) is the right tool here.
  useEffect(() => {
    if (!scrollTo) return
    virtuosoRef.current?.scrollToIndex({index: scrollTo.index, align: 'center', behavior: 'smooth'})
  }, [scrollTo])

  const context = useMemo<ListContext>(
    () => ({footer, footerRef, label: t('a11y.conversation'), onWheel: handleWheel}),
    [footer, t, handleWheel],
  )

  return (
    <div ref={containerRef} className="flex-1 overflow-hidden relative">
      <Virtuoso
        ref={virtuosoRef}
        scrollerRef={handleScrollerRef}
        data={messages}
        context={context}
        itemContent={(i, m) => <div className="chat-msg-item pb-3">{renderMessage(i, m)}</div>}
        components={COMPONENTS}
        initialTopMostItemIndex={Math.max(0, messages.length - 1)}
        // 'auto' (instant) even while streaming: followOutput's smooth
        // animation ran as a SECOND scroll system against the ResizeObserver's
        // instant jumps, and the two fighting produced the rubber-band bounce
        // at the bottom of a live reply.
        followOutput="auto"
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
