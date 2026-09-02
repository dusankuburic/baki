import {useRef, useState, type ReactNode} from 'react'
import {useDialogFocus} from '@/hooks/useDialogFocus'

// MobileDrawer is the overlay-drawer chrome for the mobile sidebar/inspector
// panes. Like the shared <Modal>, it
// provides the WCAG dialog behaviours: role=dialog + aria-modal, a Tab focus
// trap, Esc-to-close, and focus restoration to the trigger on close.
//
// The drawer mounts only while open (the parent's conditional), so isOpen is
// always true here and the hook's open-path runs on mount; the cleanup on
// unmount restores focus.
export default function MobileDrawer({
  side,
  label,
  onClose,
  children,
}: {
  side: 'left' | 'right'
  label: string
  onClose: () => void
  children: ReactNode
}) {
  const ref = useRef<HTMLDivElement>(null)
  useDialogFocus({isOpen: true, onClose, closeOnEsc: true, containerRef: ref})

  // Swipe-to-close (U5b): the drawer follows the finger along its open axis
  // and releases past 40% of its width (or a fast 80px flick). Keyboard and
  // pointer-device users keep Esc/backdrop/click paths.
  const [dragX, setDragX] = useState(0)
  const drag = useRef<{startX: number; width: number} | null>(null)
  const axisX = side === 'right' // right drawer: dragging left (negative) closes

  return (
    <div className={`fixed inset-0 z-50 flex ${side === 'right' ? 'justify-end' : ''} md:hidden`}>
      <button
        className="absolute inset-0 bg-surface-overlay/60 backdrop-blur-sm"
        onClick={onClose}
        aria-label={`Close ${label}`}
      />
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-label={label}
        style={{transform: `translateX(${dragX}px)`, touchAction: 'pan-y'}}
        onPointerDown={e => {
          drag.current = {startX: e.clientX, width: ref.current?.offsetWidth ?? 280}
          ;(e.target as HTMLElement).setPointerCapture?.(e.pointerId)
        }}
        onPointerMove={e => {
          if (!drag.current) return
          const dx = e.clientX - drag.current.startX
          // Only track the outward direction; the inward one resists at 0.
          const outward = axisX ? Math.min(0, dx) : Math.max(0, dx)
          setDragX(outward)
        }}
        onPointerUp={() => {
          const d = drag.current
          drag.current = null
          if (!d) return
          if (Math.abs(dragX) > d.width * 0.4 || Math.abs(dragX) > 80) {
            onClose()
            setDragX(0)
          } else {
            setDragX(0)
          }
        }}
        className={`relative ${side === 'right' ? 'w-80 border-l' : 'w-72 border-r'} max-w-[80vw] bg-surface-1 border-border-subtle overflow-hidden`}
      >
        {children}
      </div>
    </div>
  )
}
