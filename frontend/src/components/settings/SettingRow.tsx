import type {ReactNode} from 'react'
import clsx from 'clsx'

// One settings row: title and hint on the left, the control on the right.
// The panels used to hand-roll this shape with slightly different spacing,
// type and borders each time; this is the one version. It renders only the
// copy its caller passes in.
export default function SettingRow({
  title,
  hint,
  control,
  className,
  children,
}: {
  title: string
  hint?: string
  control?: ReactNode
  className?: string
  children?: ReactNode
}) {
  return (
    <div
      className={clsx(
        'flex items-center justify-between gap-4 rounded-lg border border-border-default bg-surface-2 px-4 py-3',
        className,
      )}
    >
      <div className="min-w-0 flex-1">
        <span className="block text-sm font-medium text-text-primary">{title}</span>
        {hint && <p className="mt-0.5 text-xs text-text-tertiary">{hint}</p>}
        {children}
      </div>
      {control && <div className="shrink-0">{control}</div>}
    </div>
  )
}
