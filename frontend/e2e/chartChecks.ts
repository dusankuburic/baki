import type {Page} from '@playwright/test'

export function chartLayoutMetrics(page: Page, scrollerSelector: string) {
  return page.evaluate(sel => {
    const scroller = document.querySelector<HTMLElement>(sel)
    const cards = Array.from(document.querySelectorAll<HTMLElement>('.chart-card, .stat-tile'))
    let ticks = 0
    let clipped = 0
    for (const svg of Array.from(document.querySelectorAll<SVGSVGElement>('svg.recharts-surface'))) {
      const box = svg.getBoundingClientRect()
      for (const tick of Array.from(svg.querySelectorAll<SVGElement>('.recharts-cartesian-axis-tick-value'))) {
        const r = tick.getBoundingClientRect()
        if (r.width === 0 && r.height === 0) continue
        ticks++
        if (
          r.left < box.left - 0.5 ||
          r.right > box.right + 0.5 ||
          r.top < box.top - 0.5 ||
          r.bottom > box.bottom + 0.5
        ) {
          clipped++
        }
      }
    }
    return {
      pageOverflow: scroller ? scroller.scrollWidth - scroller.clientWidth : -1,
      cards: cards.length,
      spilling: cards.filter(c => c.scrollWidth > c.clientWidth + 1).length,
      ticks,
      clipped,
    }
  }, scrollerSelector)
}

export const WIDTHS = [1600, 1200, 1000, 390] as const
