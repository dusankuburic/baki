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
    el.scrollIntoView({behavior: reduceMotion ? 'auto' : 'smooth', block: 'start'})
    // Land focus on the heading so the next Tab continues from the section
    // the page scrolled to, not from the nav.
    el.querySelector<HTMLElement>('h2')?.focus({preventScroll: true})
  }, [])

  return {active, jumpTo}
}
