import clsx from 'clsx'
import type {ReactNode} from 'react'

// CardShell is the shared bento-card chrome for the welcome dashboard. The grid
// placement (col-span) is supplied by the parent via className.
export function CardShell({
  title,
  action,
  className,
  children,
}: {
  title: string
  action?: ReactNode
  className?: string
  children: ReactNode
}) {
  return (
    <div className={clsx('bg-surface-2 border border-border-subtle rounded-xl p-4 shadow-sm flex flex-col', className)}>
      <div className="flex items-center justify-between mb-3">
        <h3 className="text-xs font-semibold uppercase tracking-wider text-text-tertiary">{title}</h3>
        {action}
      </div>
      <div className="flex-1 min-h-0">{children}</div>
    </div>
  )
}

// CardPlaceholder renders an honest empty/degraded state inside a card instead of
// a misleading zeroed chart.
export function CardPlaceholder({message}: {message: string}) {
  return (
    <div className="h-full w-full flex items-center justify-center text-center">
      <p className="text-sm text-text-tertiary max-w-[80%]">{message}</p>
    </div>
  )
}
