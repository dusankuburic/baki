import {useRef} from 'react'
import clsx from 'clsx'
import type {LucideIcon} from 'lucide-react'

// Tabs is the accessible tab strip (U5a.1): proper tablist/tab roles, roving
// tabindex (only the selected tab is tabbable), arrow/Home/End navigation
// with selection-follows-focus, and aria-controls wiring when a panel id is
// given. Extracted so the Sidebar and Inspector strips (previously plain
// button rows with no tab semantics) share one implementation.
export interface TabItem<T extends string> {
  value: T
  label: string
  icon?: LucideIcon
}

interface TabsProps<T extends string> {
  items: TabItem<T>[]
  value: T
  onChange: (v: T) => void
  'aria-label': string
  // When set, each tab gets aria-controls={`${panelIdPrefix}-${value}`}.
  panelIdPrefix?: string
  className?: string
}

export default function Tabs<T extends string>({
  items,
  value,
  onChange,
  'aria-label': ariaLabel,
  panelIdPrefix,
  className,
}: TabsProps<T>) {
  const stripRef = useRef<HTMLDivElement>(null)

  const focusAndSelect = (idx: number) => {
    const next = items[idx]
    if (!next) return
    onChange(next.value)
    const el = stripRef.current?.querySelector<HTMLButtonElement>(`[data-tab="${next.value}"]`)
    el?.focus()
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    const idx = items.findIndex(i => i.value === value)
    switch (e.key) {
      case 'ArrowRight':
        e.preventDefault()
        focusAndSelect((idx + 1) % items.length)
        break
      case 'ArrowLeft':
        e.preventDefault()
        focusAndSelect((idx - 1 + items.length) % items.length)
        break
      case 'Home':
        e.preventDefault()
        focusAndSelect(0)
        break
      case 'End':
        e.preventDefault()
        focusAndSelect(items.length - 1)
        break
    }
  }

  return (
    <div
      ref={stripRef}
      role="tablist"
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
      className={clsx('flex items-center gap-1', className)}
    >
      {items.map(item => {
        const Icon = item.icon
        const selected = item.value === value
        return (
          <button
            key={item.value}
            role="tab"
            type="button"
            data-tab={item.value}
            aria-selected={selected}
            tabIndex={selected ? 0 : -1}
            aria-controls={panelIdPrefix ? `${panelIdPrefix}-${item.value}` : undefined}
            title={item.label}
            className={clsx(
              'flex-1 min-w-0 overflow-hidden flex items-center justify-center gap-1.5 h-7 px-2 text-xs font-medium',
              'rounded-md transition-colors duration-fast',
              'focus-visible:ring-2 focus-visible:ring-brand-500/40 focus-visible:outline-none',
              selected
                ? 'bg-surface-3 text-text-primary shadow-xs'
                : 'text-text-tertiary hover:text-text-secondary hover:bg-surface-2',
            )}
            onClick={() => onChange(item.value)}
          >
            {Icon && <Icon size={13} className="flex-shrink-0" />}
            <span className="truncate min-w-0">{item.label}</span>
          </button>
        )
      })}
    </div>
  )
}
