import clsx from 'clsx'

const cell = 'rounded-xl border border-border-subtle bg-surface-2 p-4 shadow-sm'
const pulse = 'animate-pulse rounded-lg bg-surface-3'

// One card's shape, holding its grid slot (`className` carries the
// home-span-N) while the card's chunk is still loading. `bodyHeight` is the
// chart's own height so the row does not re-flow when the chunk lands.
// Purely decorative.
export function SkeletonCard({className, bodyHeight = 192}: {className?: string; bodyHeight?: number}) {
  return (
    <div className={clsx(cell, className)} aria-hidden="true" data-testid="home-card-skeleton">
      <div className={`${pulse} mb-3 h-4 w-32`} />
      <div className={`${pulse} w-full`} style={{height: bodyHeight}} />
    </div>
  )
}

// The Home layout's shape while the first payload loads: header, the KPI
// row, then two rows of cards. Purely decorative.
export function SkeletonDashboard() {
  return (
    <div className="home-layout" aria-hidden="true">
      <div className="mb-5">
        <div className={`${pulse} mb-2 h-7 w-56`} />
        <div className={`${pulse} h-4 w-72`} />
      </div>
      <div className="home-kpis mb-4">
        {[0, 1, 2, 3].map(i => (
          <div key={i} className={cell}>
            <div className={`${pulse} mb-2 h-3 w-16`} />
            <div className={`${pulse} mb-1 h-8 w-20`} />
            <div className={`${pulse} h-3 w-24`} />
          </div>
        ))}
      </div>
      <div className="home-grid">
        <div className={`${cell} home-span-8`}>
          <div className={`${pulse} h-48 w-full`} />
        </div>
        <div className={`${cell} home-span-4`}>
          <div className={`${pulse} h-48 w-full`} />
        </div>
        <div className={`${cell} home-span-7`}>
          <div className={`${pulse} h-48 w-full`} />
        </div>
        <div className={`${cell} home-span-5`}>
          <div className={`${pulse} h-48 w-full`} />
        </div>
      </div>
    </div>
  )
}
