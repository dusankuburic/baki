import {useRef, type ReactNode} from 'react'
import {useDialogFocus} from '@/hooks/useDialogFocus'

// MobileDrawer is the overlay-drawer chrome for the mobile sidebar/inspector
// panes. Unlike the shared <Modal> (which these previously bypassed), it
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
        className={`relative ${side === 'right' ? 'w-80 border-l' : 'w-72 border-r'} max-w-[80vw] bg-surface-1 border-border-subtle overflow-hidden`}
      >
        {children}
      </div>
    </div>
  )
}
