import {useCallback, useEffect, useRef} from 'react'

const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'

// useDialogFocus applies the three behaviours an overlay dialog needs for
// keyboard/screen-reader parity with the shared <Modal>: Tab focus trap (cycles
// within the dialog), Esc-to-close, and focus restoration to the trigger on
// close. Mobile drawers + CommandPalette + GlobalSearch don't use <Modal>, so
// they get all three from this hook (WCAG 2.4.3 / 2.1.2).
//
// containerRef points at the dialog's root element. isOpen toggles activation.
// On open, the first focusable element is auto-focused; on close, the
// previously-focused element (the trigger) is restored.
export function useDialogFocus(opts: {
  isOpen: boolean
  onClose: () => void
  closeOnEsc?: boolean
  containerRef: React.RefObject<HTMLElement | null>
}) {
  const {isOpen, onClose, closeOnEsc = true, containerRef} = opts
  const previousFocusRef = useRef<HTMLElement | null>(null)

  const handleKeyDown = useCallback(
    (e: KeyboardEvent) => {
      if (closeOnEsc && e.key === 'Escape') {
        onClose()
        return
      }
      if (e.key === 'Tab' && containerRef.current) {
        const focusable = containerRef.current.querySelectorAll<HTMLElement>(FOCUSABLE)
        if (focusable.length === 0) return
        const first = focusable[0]
        const last = focusable[focusable.length - 1]
        if (e.shiftKey && document.activeElement === first) {
          e.preventDefault()
          last.focus()
        } else if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault()
          first.focus()
        }
      }
    },
    [closeOnEsc, onClose, containerRef],
  )

  useEffect(() => {
    if (isOpen) {
      previousFocusRef.current = document.activeElement as HTMLElement
      document.addEventListener('keydown', handleKeyDown)
      const first = containerRef.current?.querySelector<HTMLElement>(FOCUSABLE)
      requestAnimationFrame(() => first?.focus())
    } else {
      document.removeEventListener('keydown', handleKeyDown)
      previousFocusRef.current?.focus()
    }
    return () => {
      document.removeEventListener('keydown', handleKeyDown)
    }
  }, [isOpen, handleKeyDown, containerRef])
}
