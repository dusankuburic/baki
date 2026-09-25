import {useCallback, useEffect, useRef, useState} from 'react'

// useActiveSection reports which of `ids` (in document order) the reader is
// in, for the profile's section nav, and jumps to one on request.
//
// A section is current while it crosses a band a quarter of the way down the
// viewport; the observer clips through the main pane's own scrollport, so no
// root needs wiring. Jumping marks the target current at once and ignores the
// observer until the scroll settles, so a short last section counts as
// current even when it never reaches the band. `ids` must be a stable array.
export function useActiveSection(ids: readonly string[]): {
  active: string
  jumpTo: (id: string) => void
} {
  const [active, setActive] = useState(ids[0] ?? '')
  const lockUntil = useRef(0)

  useEffect(() => {
    if (typeof IntersectionObserver === 'undefined') return
    const visible = new Map<string, boolean>()
    const observer = new IntersectionObserver(
      entries => {
        for (const entry of entries) visible.set((entry.target as HTMLElement).id, entry.isIntersecting)
        if (Date.now() < lockUntil.current) return
        const current = ids.find(id => visible.get(id))
        if (current) setActive(current)
      },
      {rootMargin: '-25% 0px -65% 0px'},
    )
    for (const id of ids) {
      const el = document.getElementById(id)
      if (el) observer.observe(el)
    }
    return () => observer.disconnect()
  }, [ids])

  const jumpTo = useCallback((id: string) => {
    const el = document.getElementById(id)
    if (!el) return
    lockUntil.current = Date.now() + 1000
    setActive(id)
    const reduceMotion =
      typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches
    const behavior: ScrollBehavior = reduceMotion ? 'auto' : 'smooth'
    // Scroll the pane's own scroller only. scrollIntoView would also scroll
    // every scrollable ancestor — the overflow-hidden <html> included, which
    // it can move programmatically — and shift the whole shell up, leaving a
    // blank band under it.
    const scroller = scrollParent(el)
    if (scroller && typeof scroller.scrollTo === 'function') {
      const margin = parseFloat(getComputedStyle(el).scrollMarginTop) || 0
      const top = el.getBoundingClientRect().top - scroller.getBoundingClientRect().top + scroller.scrollTop - margin
      scroller.scrollTo({top: Math.max(0, top), behavior})
    } else {
      el.scrollIntoView({behavior, block: 'start'})
    }
    // Land focus on the heading so the next Tab continues from the section
    // the page scrolled to, not from the nav.
    el.querySelector<HTMLElement>('h2')?.focus({preventScroll: true})
  }, [])

  return {active, jumpTo}
}

// The nearest ancestor that actually scrolls vertically.
function scrollParent(el: HTMLElement): HTMLElement | null {
  for (let node = el.parentElement; node; node = node.parentElement) {
    const {overflowY} = getComputedStyle(node)
    if ((overflowY === 'auto' || overflowY === 'scroll') && node.scrollHeight > node.clientHeight) return node
  }
  return null
}
