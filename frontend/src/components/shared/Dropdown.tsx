import {useState, useRef, useEffect, useCallback} from 'react'
import {type LucideIcon} from 'lucide-react'
import clsx from 'clsx'
import Kbd from './Kbd'

type DropdownItem =
  | {
      type: 'item'
      label: string
      // LucideIcon, matching IconButton — lucide-react is the app's icon set,
      // and its `size?: string | number` does not satisfy a hand-written
      // ComponentType<{size?: number}> (propTypes variance), so every icon
      // passed here failed to type-check.
      icon?: LucideIcon
      shortcut?: string
      onSelect: () => void
      danger?: boolean
      disabled?: boolean
    }
  | {type: 'separator'}
  | {type: 'group'; label: string; items: DropdownItem[]}

type DropdownProps = {
  trigger: React.ReactElement
  items: DropdownItem[]
  side?: 'top' | 'bottom' | 'left' | 'right'
  align?: 'start' | 'center' | 'end'
  className?: string
}

export type {DropdownItem}

// Accessible menu behaviour:
//   - trigger announces aria-haspopup="menu" + aria-expanded state,
//   - opening moves focus onto the first selectable item,
//   - ArrowUp/Down ROVE real DOM focus (not just a visual highlight) so
//     screen readers track the active option,
//   - closing (Escape, outside click, or selection) restores focus to the
//     trigger (don't leave focus silently dropped).
export default function Dropdown({trigger, items, side = 'bottom', align = 'start', className}: DropdownProps) {
  const [open, setOpen] = useState(false)
  const [activeIndex, setActiveIndex] = useState(-1)
  const triggerRef = useRef<HTMLDivElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)
  const [position, setPosition] = useState({top: 0, left: 0})

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
      // Reset here (and on open below) instead of in an effect: the highlight is
      // a consequence of the open/close transition, not state to re-derive after
      // one — an effect-time setState just cascades an extra render.
      setActiveIndex(-1)
      if (restoreFocus) focusTrigger()
    },
    [focusTrigger],
  )

  const toggle = useCallback(() => {
    setOpen(prev => {
      if (!prev) setActiveIndex(-1)
      return !prev
    })
  }, [])

  const updatePosition = useCallback(() => {
    if (!triggerRef.current || !menuRef.current) return
    const triggerRect = triggerRef.current.getBoundingClientRect()
    const menuRect = menuRef.current.getBoundingClientRect()

    let top = side === 'bottom' ? triggerRect.bottom + 4 : triggerRect.top - menuRect.height - 4
    let left =
      align === 'start'
        ? triggerRect.left
        : align === 'end'
          ? triggerRect.right - menuRect.width
          : triggerRect.left + (triggerRect.width - menuRect.width) / 2

    // Viewport constraints
    if (left + menuRect.width > window.innerWidth - 8) left = window.innerWidth - menuRect.width - 8
    if (left < 8) left = 8

    if (top + menuRect.height > window.innerHeight - 8) {
      // Flip to top if it fits better
      if (side === 'bottom' && triggerRect.top - menuRect.height > 8) {
        top = triggerRect.top - menuRect.height - 4
      } else {
        top = window.innerHeight - menuRect.height - 8
      }
    }
    if (top < 8) top = 8

    setPosition({top, left})
  }, [side, align])

  useEffect(() => {
    if (!open) return
    updatePosition()
    // The menu is position:fixed, so it does NOT follow its trigger when an
    // ancestor scrolls — it just floats away. Capture-phase scroll catches
    // scrolling containers, not only the window. (SourceFilePicker already did
    // this; Dropdown positioned once on open and never again.)
    window.addEventListener('resize', updatePosition)
    window.addEventListener('scroll', updatePosition, true)
    return () => {
      window.removeEventListener('resize', updatePosition)
      window.removeEventListener('scroll', updatePosition, true)
    }
  }, [open, updatePosition])

  // Roving focus: whenever the active item changes while open, move real DOM
  // focus onto it so SRs announce the highlighted option.
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
      const selectable = flattenedItems.filter(i => !i.disabled)
      if (e.key === 'ArrowDown') {
        e.preventDefault()
        setActiveIndex(prev => {
          const idx = selectable.findIndex(i => i._index === prev)
          return idx < selectable.length - 1 ? selectable[idx + 1]._index : selectable[0]._index
        })
      } else if (e.key === 'ArrowUp') {
        e.preventDefault()
        setActiveIndex(prev => {
          const idx = selectable.findIndex(i => i._index === prev)
          return idx > 0 ? selectable[idx - 1]._index : selectable[selectable.length - 1]._index
        })
      } else if (e.key === 'Home') {
        e.preventDefault()
        if (selectable.length) setActiveIndex(selectable[0]._index)
      } else if (e.key === 'End') {
        e.preventDefault()
        if (selectable.length) setActiveIndex(selectable[selectable.length - 1]._index)
      } else if (e.key === 'Enter' && activeIndex >= 0) {
        // Resolve by _index rather than array position: they coincide now, but
        // an identity lookup cannot silently drift again.
        const item = flattenedItems.find(i => i._index === activeIndex)
        if (item && !item.disabled) {
          item.onSelect()
          close(true)
        }
      } else if (e.key === 'Escape') {
        close(true)
      } else if (e.key === 'Tab') {
        // Let Tab leave the menu naturally; just close without restoring
        // focus (the browser moves it anyway).
        setOpen(false)
      }
    },
    [open, activeIndex, flattenedItems, close],
  )

  return (
    <div className={clsx('inline-block', className)} onKeyDown={handleKeyDown}>
      <div ref={triggerRef} onClick={toggle} aria-haspopup="menu" aria-expanded={open}>
        {trigger}
      </div>
      {open && (
        <div
          ref={menuRef}
          className="fixed z-overlay bg-surface-2 border border-border-default rounded-lg shadow-lg py-1 min-w-[180px] animate-fade-in"
          style={{top: position.top, left: position.left}}
          role="menu"
          aria-activedescendant={activeIndex >= 0 ? `menu-item-${activeIndex}` : undefined}
          tabIndex={-1}
          onKeyDown={e => {
            if (e.key === 'Escape') close(true)
          }}
        >
          {/* eslint-disable-next-line react-hooks/refs -- `close` reaches
              triggerRef.current, but only when this callback is INVOKED from a
              click/keypress; the rule flags its mere construction in render. */}
          {renderItems(items, activeIndex, () => close(true), {n: 0})}
        </div>
      )}
    </div>
  )
}

type SelectableItem = Extract<DropdownItem, {type: 'item'}>
type FlattenedDropdownItem = SelectableItem & {_index: number}

// flattenItems numbers the SELECTABLE items in render order, and nothing else.
//
// It previously advanced the counter for `group` wrappers and separators too,
// so `_index` drifted from the flattened array position as soon as a group was
// present — and the Enter handler, which used activeIndex as an array index,
// then fired the wrong item's onSelect (or none). Counting only `type: 'item'`
// entries keeps `_index` in lockstep with renderItems' identical DFS walk,
// which is what makes data-index / aria-activedescendant resolvable.
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

// `counter` is threaded through the recursion so nested groups continue the same
// numbering flattenItems produces. Previously renderItems was handed the ORIGINAL
// nested array, whose entries carry no `_index` at all — so `data-index` and the
// `menu-item-N` ids were never emitted, `isActive` compared against undefined,
// and the roving-focus effect's `dataset.index` lookup could never match. Result:
// keyboard navigation moved no real DOM focus and highlighted nothing, in EVERY
// dropdown, grouped or not.
function renderItems(
  items: DropdownItem[],
  activeIndex: number,
  close: () => void,
  counter: {n: number},
): React.ReactNode {
  return items.map((item, i) => {
    if (item.type === 'separator') {
      return <div key={i} className="my-1 mx-2 h-px bg-border-subtle" />
    }
    if (item.type === 'group') {
      return (
        <div key={i}>
          <div className="text-2xs uppercase tracking-wider text-text-tertiary px-2 py-1">{item.label}</div>
          {renderItems(item.items, activeIndex, close, counter)}
        </div>
      )
    }
    const Icon = item.icon
    const index = counter.n++
    const isActive = index === activeIndex
    return (
      <button
        key={i}
        data-index={index}
        id={`menu-item-${index}`}
        tabIndex={-1}
        className={clsx(
          'w-full flex items-center h-8 px-2 text-sm text-left transition-colors duration-fast',
          isActive ? 'bg-surface-3 border-l-2 border-brand-500' : 'border-l-2 border-transparent',
          item.danger ? 'text-semantic-error hover:bg-semantic-error/10' : 'text-text-primary hover:bg-surface-3',
          item.disabled && 'opacity-50 cursor-not-allowed pointer-events-none',
        )}
        onClick={() => {
          if (!item.disabled) {
            item.onSelect()
            close()
          }
        }}
        role="menuitem"
        aria-disabled={item.disabled || undefined}
      >
        {Icon && <Icon size={16} className="mr-2 text-text-tertiary flex-shrink-0" />}
        <span className="flex-1">{item.label}</span>
        {item.shortcut && <Kbd keys={[item.shortcut]} size="xs" className="ml-4" />}
      </button>
    )
  })
}
