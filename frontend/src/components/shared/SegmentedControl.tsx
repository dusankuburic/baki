import clsx from 'clsx'
import {type LucideIcon} from 'lucide-react'

type SegmentedControlProps<T> = {
  value: T
  onChange: (value: T) => void
  options: {value: T; label: string; icon?: LucideIcon}[]
  size?: 'sm' | 'md'
  className?: string
  /**
   * Renders the group read-only: buttons are non-interactive and marked
   * aria-disabled, but the SELECTED value stays visible. Parity with Switch,
   * which already has this — a read-only viewer needs to see the current
   * setting, not an empty control.
   */
  disabled?: boolean
}

export default function SegmentedControl<T extends string>({
  value,
  onChange,
  options,
  size = 'md',
  className,
  disabled = false,
}: SegmentedControlProps<T>) {
  return (
    <div
      className={clsx('inline-flex bg-surface-2 border border-border-default rounded-md p-0.5 gap-0.5', className)}
      role="radiogroup"
    >
      {options.map(opt => {
        const Icon = opt.icon
        const isActive = opt.value === value
        return (
          <button
            key={opt.value}
            className={clsx(
              'px-3 rounded-sm transition-colors duration-fast text-sm',
              disabled ? 'cursor-not-allowed opacity-60' : 'cursor-pointer',
              size === 'sm' ? 'h-6' : 'h-7',
              isActive
                ? 'bg-surface-3 text-text-primary shadow-xs'
                : clsx('text-text-secondary', !disabled && 'hover:text-text-primary'),
            )}
            onClick={() => {
              if (!disabled) onChange(opt.value)
            }}
            role="radio"
            aria-checked={isActive}
            aria-disabled={disabled || undefined}
          >
            <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
              {Icon && <Icon size={14} />}
              {opt.label}
            </span>
          </button>
        )
      })}
    </div>
  )
}
