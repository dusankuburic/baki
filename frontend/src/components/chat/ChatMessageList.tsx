import {useTranslation} from 'react-i18next'
import React, {useRef, useEffect, useState, useMemo, useCallback, memo} from 'react'
import {Virtuoso, type VirtuosoHandle} from 'react-virtuoso'
import {ChevronDown} from 'lucide-react'
import type {ChatMessage} from '@/types'

interface Props {
  messages: ChatMessage[]
  renderMessage: (index: number, message: ChatMessage) => React.ReactNode
  footer?: React.ReactNode
  isStreaming?: boolean
  scrollTo?: {index: number; nonce: number}
}

interface ListContext {
  footer?: React.ReactNode
  footerRef: React.MutableRefObject<HTMLDivElement | null>
  label: string
  onWheel: () => void
}

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

const AT_BOTTOM_PX = 24

function distanceFromBottom(el: HTMLElement): number {
  return el.scrollHeight - el.clientHeight - el.scrollTop
}

function ChatMessageList({messages, renderMessage, footer, isStreaming, scrollTo}: Props) {
  const {t} = useTranslation('chat')
  const virtuosoRef = useRef<VirtuosoHandle>(null)
  const containerRef = useRef<HTMLDivElement | null>(null)
  const scrollerRef = useRef<HTMLElement | null>(null)
  const footerRef = useRef<HTMLDivElement | null>(null)
  const [showScrollBtn, setShowScrollBtn] = useState(false)
  const atBottomRef = useRef(true)
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

  useEffect(() => {
    if (!isStreaming) return
    atBottomRef.current = true
    const raf = requestAnimationFrame(() => scrollToBottom(false))
    return () => cancelAnimationFrame(raf)
  }, [isStreaming, scrollToBottom])

  useEffect(() => {
    if (!isStreaming) return
    const el = footerRef.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(() => {
      if (Date.now() - lastWheelRef.current < 400) return
      if (atBottomRef.current) scrollToBottom(false)
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [isStreaming, scrollToBottom])

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

export default memo(ChatMessageList)
