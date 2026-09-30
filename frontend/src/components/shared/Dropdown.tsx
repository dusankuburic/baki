import {useState, useRef, useEffect, useCallback, useId, cloneElement} from 'react'
import {type LucideIcon} from 'lucide-react'
import clsx from 'clsx'
import Kbd from './Kbd'
import {useAnchoredPosition} from '@/hooks/useAnchoredPosition'

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
      // Unavailable AND skipped by the arrow keys.
      disabled?: boolean
      // Unavailable, but it says why: the item stays in the arrow-key order,
      // is marked aria-disabled, and shows this text on a second line, which
      // is also its accessible description. Use it wherever a user would
      // otherwise wonder where an action went ("needs credentials").
      disabledReason?: string
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
//   - the trigger itself announces aria-haspopup="menu", aria-expanded and
//     (while open) aria-controls. They are cloned onto the trigger element:
//     on the wrapper <div> they sat on a node no screen reader focuses, so
//     the button announced none of them. A trigger component must pass
//     aria-* props through to its <button> (Button and IconButton do),
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
      // A disabledReason item is unavailable but still reachable, so its
      // reason can be read; only a plain `disabled` one is skipped.
      const selectable = flattenedItems.filter(i => !isSkipped(i))
      if ((e.key === 'ArrowDown' || e.key === 'ArrowUp') && selectable.length === 0) {
        e.preventDefault()
      } else if (e.key === 'ArrowDown') {
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
        if (item && !isUnavailable(item)) {
          item.onSelect()
          close(true)
        }
      } else if (e.key === 'Escape') {
        // Claimed: the same keypress used to reach the chat's Escape binding
        // and stop a reply just because a menu was closed.
        e.preventDefault()
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

// Unavailable items never run onSelect. A plain `disabled` one is also left
// out of the arrow-key order; one with a disabledReason stays in it.
const isUnavailable = (item: SelectableItem) => !!item.disabled || !!item.disabledReason
const isSkipped = (item: SelectableItem) => !!item.disabled && !item.disabledReason

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
            close()
          }
        }}
        role="menuitem"
        aria-disabled={unavailable || undefined}
        aria-describedby={reasonId}
      >
        {Icon && <Icon size={16} className="mr-2 text-text-tertiary flex-shrink-0" />}
        <span className="flex-1">
          {item.label}
          {/* aria-hidden keeps the reason out of the item's NAME; the
              aria-describedby reference still reads it as the description. */}
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
}
