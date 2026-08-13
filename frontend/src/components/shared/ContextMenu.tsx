import {useEffect, useRef} from 'react'
import clsx from 'clsx'
import type {LucideIcon} from 'lucide-react'
import {useDialogFocus} from '@/hooks/useDialogFocus'

export interface ContextMenuItem {
  label: string
  icon: LucideIcon
  onClick: () => void
  variant?: 'default' | 'danger'
}

interface ContextMenuProps {
  x: number
  y: number
  onClose: () => void
  items: ContextMenuItem[]
}

// ContextMenu previously had no a11y semantics (no role, no Esc, no focus
// management): a keyboard/screen-reader user couldn't tell it was a menu, Esc
// didn't close it, and opening it left focus wherever it was. Now it carries
// role=menu / role=menuitem, closes on Esc, auto-focuses the first item, and
// restores focus to the trigger on close (via the shared useDialogFocus hook).
export default function ContextMenu({x, y, onClose, items}: ContextMenuProps) {
  const ref = useRef<HTMLDivElement>(null)
  useDialogFocus({isOpen: true, onClose, containerRef: ref})

  useEffect(() => {
    const handleClick = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        onClose()
      }
    }
    window.addEventListener('mousedown', handleClick)
    return () => window.removeEventListener('mousedown', handleClick)
  }, [onClose])

  // Adjust position if menu goes off-screen
  const left = Math.min(x, window.innerWidth - 200)
  const top = Math.min(y, window.innerHeight - (items.length * 32 + 20))

  return (
    <div
      ref={ref}
      role="menu"
      aria-label="Context menu"
      className="fixed z-tooltip min-w-[180px] bg-surface-1 border border-border-default rounded-lg shadow-xl py-1.5 animate-fade-in"
      style={{left, top}}
    >
      {items.map((item, i) => {
        const Icon = item.icon
        return (
          <button
            key={i}
            role="menuitem"
            className={clsx(
              'w-full flex items-center gap-2.5 px-3 py-1.5 text-xs font-medium transition-colors text-left',
              item.variant === 'danger'
                ? 'text-semantic-error hover:bg-semantic-error/10'
                : 'text-text-secondary hover:bg-surface-3 hover:text-text-primary',
            )}
            onClick={e => {
              e.stopPropagation()
              item.onClick()
              onClose()
            }}
          >
            <Icon size={14} className="opacity-70" />
            {item.label}
          </button>
        )
      })}
    </div>
  )
}
