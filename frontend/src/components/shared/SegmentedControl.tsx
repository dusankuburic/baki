import {useRef, useState} from 'react'
import clsx from 'clsx'
import {type LucideIcon} from 'lucide-react'

export type SegmentedOption<T> = {
  value: T
  label: string
  icon?: LucideIcon
  disabled?: boolean
}

type SegmentedControlProps<T> = {
  value: T
  onChange: (value: T) => void
  options: SegmentedOption<T>[]
  size?: 'sm' | 'md'
  className?: string
  disabled?: boolean
  label?: string
  labelledBy?: string
  describedBy?: string
  collapseLabels?: boolean
  activation?: 'auto' | 'manual'
  fullWidth?: boolean
}

export default function SegmentedControl<T extends string>({
  value,
  onChange,
  options,
  size = 'md',
  className,
  disabled = false,
  label,
  labelledBy,
  describedBy,
  collapseLabels = false,
  activation = 'auto',
  fullWidth = false,
}: SegmentedControlProps<T>) {
  const groupRef = useRef<HTMLDivElement>(null)
  const [focused, setFocused] = useState<T | null>(null)

  const has = (v: T | null) => v !== null && options.some(o => o.value === v)
  const tabStop = has(focused)
    ? focused
    : has(value)
      ? value
      : (options.find(o => !o.disabled)?.value ?? options[0]?.value)

  const onKeyDown = (e: React.KeyboardEvent<HTMLButtonElement>, index: number) => {
    const reachable = options.flatMap((o, i) => (o.disabled ? [] : [i]))
    if (reachable.length === 0) return
    const at = reachable.indexOf(index)
    let target: number | undefined
    switch (e.key) {
      case 'ArrowRight':
      case 'ArrowDown':
        target = at === -1 ? (reachable.find(i => i > index) ?? reachable[0]) : reachable[(at + 1) % reachable.length]
        break
      case 'ArrowLeft':
      case 'ArrowUp':
        target =
          at === -1
            ? ([...reachable].reverse().find(i => i < index) ?? reachable[reachable.length - 1])
            : reachable[(at - 1 + reachable.length) % reachable.length]
        break
      case 'Home':
        target = reachable[0]
        break
      case 'End':
        target = reachable[reachable.length - 1]
        break
      default:
        return
    }
    if (target === undefined) return
    e.preventDefault()
    groupRef.current?.querySelectorAll<HTMLButtonElement>('[role="radio"]')[target]?.focus()
    const next = options[target]
    if (activation === 'auto' && !disabled && next && next.value !== value) onChange(next.value)
  }

  return (
    <div
      ref={groupRef}
      className={clsx(
        fullWidth ? 'flex w-full' : 'inline-flex',
        'bg-surface-2 border border-border-default rounded-md p-0.5 gap-0.5',
        collapseLabels && 'seg-collapsible',
        className,
      )}
      role="radiogroup"
      aria-label={label}
      aria-labelledby={labelledBy}
      aria-describedby={describedBy}
      onBlur={e => {
        if (!groupRef.current?.contains(e.relatedTarget as Node | null)) setFocused(null)
      }}
    >
      {options.map((opt, index) => {
        const Icon = opt.icon
        const isActive = opt.value === value
        const unavailable = disabled || !!opt.disabled
        return (
          <button
            key={opt.value}
            type="button"
            className={clsx(
              'px-3 rounded-sm transition-colors duration-fast text-sm',
              fullWidth && 'flex-1 min-w-0',
              unavailable ? 'cursor-not-allowed opacity-60' : 'cursor-pointer',
              size === 'sm' ? 'h-6' : 'h-7',
              isActive
                ? 'bg-surface-3 text-text-primary shadow-xs'
                : clsx('text-text-secondary', !unavailable && 'hover:text-text-primary'),
            )}
            onClick={() => {
              if (!unavailable) onChange(opt.value)
            }}
            onKeyDown={e => onKeyDown(e, index)}
            onFocus={() => setFocused(opt.value)}
            role="radio"
            aria-checked={isActive}
            aria-disabled={unavailable || undefined}
            tabIndex={opt.value === tabStop ? 0 : -1}
            aria-label={collapseLabels ? opt.label : undefined}
            title={collapseLabels ? opt.label : undefined}
          >
            <span className={clsx('inline-flex items-center gap-1.5 whitespace-nowrap', fullWidth && 'justify-center')}>
              {Icon && <Icon size={14} aria-hidden="true" />}
              {collapseLabels ? <span className="seg-label">{opt.label}</span> : opt.label}
            </span>
          </button>
        )
      })}
    </div>
  )
}
