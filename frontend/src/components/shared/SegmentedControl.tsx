import {useRef, useState} from 'react'
import clsx from 'clsx'
import {type LucideIcon} from 'lucide-react'

export type SegmentedOption<T> = {
  value: T
  label: string
  icon?: LucideIcon
  /**
   * Shown but not selectable: marked aria-disabled and skipped by the arrow
   * keys. Say why next to the control and point `describedBy` at it.
   */
  disabled?: boolean
}

type SegmentedControlProps<T> = {
  value: T
  onChange: (value: T) => void
  options: SegmentedOption<T>[]
  size?: 'sm' | 'md'
  className?: string
  /**
   * Renders the group read-only: buttons are non-interactive and marked
   * aria-disabled, but the SELECTED value stays visible. Parity with Switch,
   * which already has this — a read-only viewer needs to see the current
   * setting, not an empty control.
   */
  disabled?: boolean
  /** Accessible name for the group. */
  label?: string
  /** Id of a visible element that names the group (wins over `label`). */
  labelledBy?: string
  /** Id(s) of a hint or error that describes the group. */
  describedBy?: string
  /**
   * Lets a container query hide the option labels (class `seg-label`) and
   * keep the icons: each button then carries its label as aria-label and
   * title, so an icon-only segment keeps its name and a hover hint.
   */
  collapseLabels?: boolean
  /**
   * What the arrow keys do. The group is one tab stop either way, and
   * Arrow/Home/End move between the options:
   *   'auto'   — moving also selects (the radio-group default);
   *   'manual' — moving only focuses; Space or Enter selects.
   * Use 'manual' where a change saves a setting or re-renders a lot, so
   * arrowing across the options does not apply every one of them.
   */
  activation?: 'auto' | 'manual'
  /** Stretch across the container, the options sharing its width. */
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
  // The option that has focus while focus is inside the group. It holds the
  // tab stop: with manual activation the focused option is not the selected
  // one, and Shift+Tab from it must leave the group, not land on the
  // selected option (which a tabIndex of 0 would make the previous stop).
  const [focused, setFocused] = useState<T | null>(null)

  const has = (v: T | null) => v !== null && options.some(o => o.value === v)
  // Roving tabindex: one tab stop — the focused option, else the selected
  // one, else the first option that can be selected.
  const tabStop = has(focused)
    ? focused
    : has(value)
      ? value
      : (options.find(o => !o.disabled)?.value ?? options[0]?.value)

  const onKeyDown = (e: React.KeyboardEvent<HTMLButtonElement>, index: number) => {
    // Indexes of the options the arrows can reach, in order.
    const reachable = options.flatMap((o, i) => (o.disabled ? [] : [i]))
    if (reachable.length === 0) return
    const at = reachable.indexOf(index)
    let target: number | undefined
    switch (e.key) {
      case 'ArrowRight':
      case 'ArrowDown':
        // From an unreachable option (a disabled one that is selected, so it
        // holds the tab stop) step to the next reachable one after it.
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
    // reachable is non-empty (early return above), so target is always set;
    // the guard only satisfies the index-access type.
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
