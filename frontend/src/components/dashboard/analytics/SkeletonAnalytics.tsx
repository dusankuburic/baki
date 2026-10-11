export function SkeletonAnalytics() {
  const cell = 'rounded-xl border border-border-subtle bg-surface-2 p-4 shadow-sm'
  const pulse = 'animate-pulse rounded-lg bg-surface-3'
  return (
    <div aria-hidden="true">
      <div className="mb-4 flex flex-wrap gap-2">
        <div className={`${pulse} h-8 w-64`} />
        <div className={`${pulse} h-8 w-32`} />
        <div className={`${pulse} h-8 w-28`} />
      </div>
      <div className="analytics-tiles mb-6">
        {[0, 1, 2, 3, 4, 5].map(i => (
          <div key={i} className={cell}>
            <div className={`${pulse} mb-2 h-3 w-16`} />
            <div className={`${pulse} mb-1 h-8 w-20`} />
            <div className={`${pulse} h-3 w-24`} />
          </div>
        ))}
      </div>
      <div className="analytics-grid">
        <div className={`${cell} analytics-span-8`}>
          <div className={`${pulse} h-52 w-full`} />
        </div>
        <div className={`${cell} analytics-span-4`}>
          <div className={`${pulse} h-52 w-full`} />
        </div>
        <div className={`${cell} analytics-span-6`}>
          <div className={`${pulse} h-52 w-full`} />
        </div>
        <div className={`${cell} analytics-span-6`}>
          <div className={`${pulse} h-52 w-full`} />
        </div>
      </div>
    </div>
  )
}
