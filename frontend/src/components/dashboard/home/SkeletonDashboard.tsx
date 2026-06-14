// SkeletonDashboard mirrors the real grid's col-spans so the layout doesn't
// shift (CLS) when the data arrives. Pure presentational; no data.
export function SkeletonDashboard() {
  const cell = 'bg-surface-2 border border-border-subtle rounded-xl p-4 shadow-sm'
  const pulse = 'animate-pulse bg-surface-3 rounded-lg'
  return (
    <div className="grid grid-cols-12 gap-4 max-w-7xl mx-auto" aria-hidden="true">
      <div className="col-span-12 flex items-center justify-between mb-2">
        <div className={`${pulse} h-7 w-56`} />
        <div className={`${pulse} h-7 w-32`} />
      </div>
      <div className={`${cell} col-span-12 lg:col-span-4`}>
        <div className={`${pulse} h-48 w-full`} />
      </div>
      <div className={`${cell} col-span-12 lg:col-span-8`}>
        <div className={`${pulse} h-48 w-full`} />
      </div>
      <div className={`${cell} col-span-12 lg:col-span-7`}>
        <div className={`${pulse} h-48 w-full`} />
      </div>
      <div className={`${cell} col-span-12 lg:col-span-5`}>
        <div className={`${pulse} h-48 w-full`} />
      </div>
    </div>
  )
}
