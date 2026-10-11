import {useCallback, useEffect, useRef, useState} from 'react'

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
    const scroller = scrollParent(el)
    if (scroller && typeof scroller.scrollTo === 'function') {
      const margin = parseFloat(getComputedStyle(el).scrollMarginTop) || 0
      const top = el.getBoundingClientRect().top - scroller.getBoundingClientRect().top + scroller.scrollTop - margin
      scroller.scrollTo({top: Math.max(0, top), behavior})
    } else {
      el.scrollIntoView({behavior, block: 'start'})
    }
    el.querySelector<HTMLElement>('h2')?.focus({preventScroll: true})
  }, [])

  return {active, jumpTo}
}

function scrollParent(el: HTMLElement): HTMLElement | null {
  for (let node = el.parentElement; node; node = node.parentElement) {
    const {overflowY} = getComputedStyle(node)
    if ((overflowY === 'auto' || overflowY === 'scroll') && node.scrollHeight > node.clientHeight) return node
  }
  return null
}
