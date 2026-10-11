import {useState, useRef, useEffect, useCallback, useId, cloneElement} from 'react'
import {type LucideIcon} from 'lucide-react'
import clsx from 'clsx'
import Kbd from './Kbd'
import {useAnchoredPosition} from '@/hooks/useAnchoredPosition'

type DropdownItem =
  | {
      type: 'item'
      label: string
      icon?: LucideIcon
      shortcut?: string
      onSelect: () => void
      danger?: boolean
      disabled?: boolean
      disabledReason?: string
    }
  | {type: 'separator'}
  | {type: 'group'; label: string; items: DropdownItem[]}

type DropdownProps = {
  trigger: React.ReactElement<Record<string, unknown>>
  items: DropdownItem[]
  side?: 'top' | 'bottom' | 'left' | 'right'
  align?: 'start' | 'center' | 'end'
  className?: string
}

export type {DropdownItem}

export default function Dropdown({trigger, items, side = 'bottom', align = 'start', className}: DropdownProps) {
  const [open, setOpen] = useState(false)
  const [activeIndex, setActiveIndex] = useState(-1)
  const triggerRef = useRef<HTMLDivElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)
  const menuId = useId()
  const position = useAnchoredPosition({open, anchorRef: triggerRef, floatingRef: menuRef, side, align})

  const flattenedItems = flattenItems(items)

  const selectableButtons = () =>
    menuRef.current
      ? Array.from(menuRef.current.querySelectorAll<HTMLButtonElement>('button[role="menuitem"]:not([disabled])'))
      : []

  const focusTrigger = useCallback(() => {
    const el = triggerRef.current?.querySelector<HTMLElement>('button, [tabindex]:not([tabindex="-1"])')
    el?.focus()
  }, [])

  const close = useCallback(
    (restoreFocus: boolean) => {
      setOpen(false)
      setActiveIndex(-1)
      if (restoreFocus) focusTrigger()
    },
    [focusTrigger],
  )

  const closeFromItem = useCallback(() => close(true), [close])

  const renderItems = (items: DropdownItem[], activeIndex: number, counter: {n: number}): React.ReactNode =>
    items.map((item, i) => {
      if (item.type === 'separator') {
        return <div key={i} className="my-1 mx-2 h-px bg-border-subtle" />
      }
      if (item.type === 'group') {
        return (
          <div key={i}>
            <div className="text-2xs uppercase tracking-wider text-text-tertiary px-2 py-1">{item.label}</div>
            {renderItems(item.items, activeIndex, counter)}
          </div>
        )
      }
      const Icon = item.icon
      const index = counter.n++
      const isActive = index === activeIndex
      const unavailable = isUnavailable(item)
      const reasonId = item.disabledReason ? `menu-item-${index}-reason` : undefined
      return (
        <button
          key={i}
          type="button"
          data-index={index}
          id={`menu-item-${index}`}
          tabIndex={-1}
          className={clsx(
            'w-full flex items-center px-2 text-sm text-left transition-colors duration-fast',
            item.disabledReason ? 'min-h-8 py-1' : 'h-8',
            isActive ? 'bg-surface-3 border-l-2 border-brand-500' : 'border-l-2 border-transparent',
            item.disabledReason
              ? 'text-text-tertiary cursor-not-allowed'
              : item.danger
                ? 'text-semantic-error hover:bg-semantic-error/10'
                : 'text-text-primary hover:bg-surface-3',
            isSkipped(item) && 'opacity-50 cursor-not-allowed pointer-events-none',
          )}
          onClick={() => {
            if (!unavailable) {
              item.onSelect()
              closeFromItem()
            }
          }}
          role="menuitem"
          aria-disabled={unavailable || undefined}
          aria-describedby={reasonId}
        >
          {Icon && <Icon size={16} className="mr-2 text-text-tertiary shrink-0" />}
          <span className="flex-1">
            {item.label}
            {item.disabledReason && (
              <span id={reasonId} aria-hidden="true" className="block text-2xs">
                {item.disabledReason}
              </span>
            )}
          </span>
          {item.shortcut && <Kbd keys={[item.shortcut]} size="xs" className="ml-4" />}
        </button>
      )
    })

  const toggle = useCallback(() => {
    setOpen(prev => {
      if (!prev) setActiveIndex(-1)
      return !prev
    })
  }, [])

  useEffect(() => {
    if (!open || activeIndex < 0) return
    const btns = selectableButtons()
    const target = btns.find(b => b.dataset.index === String(activeIndex))
    target?.focus()
  }, [open, activeIndex])

  useEffect(() => {
    if (!open) return
    const handler = (e: MouseEvent) => {
      if (!triggerRef.current?.contains(e.target as Node) && !menuRef.current?.contains(e.target as Node)) {
        close(false)
      }
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [open, close])

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (!open) return
      const selectable = flattenedItems.filter(i => !isSkipped(i))
      if ((e.key === 'ArrowDown' || e.key === 'ArrowUp') && selectable.length === 0) {
        e.preventDefault()
      } else if (e.key === 'ArrowDown') {
        e.preventDefault()
        setActiveIndex(prev => {
          const idx = selectable.findIndex(i => i._index === prev)
          const next = idx < selectable.length - 1 ? selectable[idx + 1] : selectable[0]
          return next ? next._index : prev
        })
      } else if (e.key === 'ArrowUp') {
        e.preventDefault()
        setActiveIndex(prev => {
          const idx = selectable.findIndex(i => i._index === prev)
          const next = idx > 0 ? selectable[idx - 1] : selectable[selectable.length - 1]
          return next ? next._index : prev
        })
      } else if (e.key === 'Home') {
        e.preventDefault()
        const first = selectable[0]
        if (first) setActiveIndex(first._index)
      } else if (e.key === 'End') {
        e.preventDefault()
        const last = selectable[selectable.length - 1]
        if (last) setActiveIndex(last._index)
      } else if (e.key === 'Enter' && activeIndex >= 0) {
        const item = flattenedItems.find(i => i._index === activeIndex)
        if (item && !isUnavailable(item)) {
          item.onSelect()
          close(true)
        }
      } else if (e.key === 'Escape') {
        e.preventDefault()
        close(true)
      } else if (e.key === 'Tab') {
        setOpen(false)
      }
    },
    [open, activeIndex, flattenedItems, close],
  )

  return (
    <div className={clsx('inline-block', className)} onKeyDown={handleKeyDown}>
      <div ref={triggerRef} onClick={toggle}>
        {cloneElement(trigger, {
          'aria-haspopup': 'menu',
          'aria-expanded': open,
          'aria-controls': open ? menuId : undefined,
        })}
      </div>
      {open && (
        <div
          ref={menuRef}
          id={menuId}
          className="fixed z-overlay bg-surface-2 border border-border-default rounded-lg shadow-lg py-1 min-w-[180px] animate-fade-in"
          style={{top: position.top, left: position.left}}
          role="menu"
          aria-activedescendant={activeIndex >= 0 ? `menu-item-${activeIndex}` : undefined}
          tabIndex={-1}
          onKeyDown={e => {
            if (e.key === 'Escape') {
              e.preventDefault()
              close(true)
            }
          }}
        >
          {renderItems(items, activeIndex, {n: 0})}
        </div>
      )}
    </div>
  )
}

type SelectableItem = Extract<DropdownItem, {type: 'item'}>
type FlattenedDropdownItem = SelectableItem & {_index: number}

const isUnavailable = (item: SelectableItem) => !!item.disabled || !!item.disabledReason
const isSkipped = (item: SelectableItem) => !!item.disabled && !item.disabledReason

function flattenItems(items: DropdownItem[]): FlattenedDropdownItem[] {
  const result: FlattenedDropdownItem[] = []
  function walk(list: DropdownItem[]) {
    for (const item of list) {
      if (item.type === 'group') walk(item.items)
      else if (item.type === 'item') result.push({...item, _index: result.length})
    }
  }
  walk(items)
  return result
}

