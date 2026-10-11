import clsx from 'clsx'

export function Meter({
  value,
  max,
  label,
  token = '--brand-500',
  className,
}: {
  value: number
  max: number
  label: string
  token?: string
  className?: string
}) {
  const pct = max > 0 ? Math.min(100, Math.max(0, (value / max) * 100)) : 0
  return (
    <div
      role="meter"
      aria-label={label}
      aria-valuenow={value}
      aria-valuemin={0}
      aria-valuemax={max}
      className={clsx('h-2 w-full overflow-hidden rounded-full', className)}
      style={{background: `color-mix(in srgb, var(${token}) 15%, transparent)`}}
    >
      <div
        className="h-full rounded-full transition-[width] duration-normal"
        style={{width: `${pct}%`, background: `var(${token})`}}
      />
    </div>
  )
}
