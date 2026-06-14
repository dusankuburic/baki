export function SkeletonDashboard() {
  const cell = 'bg-surface-2 border border-border-subtle rounded-xl p-4 shadow-sm'
  const pulse = 'animate-pulse bg-surface-3 rounded-lg'
  return (
    <div className="grid grid-cols-12 gap-4 max-w-7xl mx-auto" aria-hidden="true">
      {/* Header */}
      <div className="col-span-12 mb-1">
        <div className={`${pulse} h-7 w-56 mb-2`} />
        <div className={`${pulse} h-4 w-72`} />
      </div>
      {/* KPI Strip */}
      <div className="col-span-12 grid grid-cols-2 lg:grid-cols-4 gap-3">
        {[0, 1, 2, 3].map(i => (
          <div key={i} className={`${cell}`}>
            <div className={`${pulse} h-3 w-16 mb-2`} />
            <div className={`${pulse} h-8 w-20 mb-1`} />
            <div className={`${pulse} h-3 w-24`} />
          </div>
        ))}
      </div>
      {/* Row 2 */}
      <div className={`${cell} col-span-12 lg:col-span-8`}>
        <div className={`${pulse} h-48 w-full`} />
      </div>
      <div className={`${cell} col-span-12 lg:col-span-4`}>
        <div className={`${pulse} h-48 w-full`} />
      </div>
      {/* Row 3 */}
      <div className={`${cell} col-span-12 lg:col-span-8`}>
        <div className={`${pulse} h-48 w-full`} />
      </div>
      <div className={`${cell} col-span-12 lg:col-span-4`}>
        <div className={`${pulse} h-48 w-full`} />
      </div>
    </div>
  )
}
