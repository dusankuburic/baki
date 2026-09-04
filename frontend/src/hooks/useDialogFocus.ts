import {useCallback, useEffect, useRef} from 'react'

export const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'

// useDialogFocus applies the three behaviours an overlay dialog needs for
// keyboard/screen-reader parity: Tab focus trap (cycles within the dialog),
// Esc-to-close, and focus restoration to the trigger on close. Mobile drawers,
// CommandPalette, GlobalSearch and the shared <Modal> all get all three from
// here (WCAG 2.4.3 / 2.1.2) — <Modal> used to carry its own byte-identical copy,
// which meant fixing a trap bug required finding both.
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
      // defaultPrevented: a nested widget inside the dialog (an autocomplete
      // menu, a combobox) handles Escape first to dismiss ITSELF. Without this
      // check the same keypress also tore down the whole dialog, so one Escape
      // closed both the menu and the window it was opened from.
      if (closeOnEsc && e.key === 'Escape' && !e.defaultPrevented) {
        onClose()
        return
      }
      if (e.key === 'Tab' && containerRef.current) {
        // :not([disabled]) — a disabled control is not tabbable, so including it
        // as the trap's first/last boundary sends focus to an element the
        // browser immediately skips, breaking the cycle.
        const focusable = containerRef.current.querySelectorAll<HTMLElement>(FOCUSABLE)
        const tabbable = Array.from(focusable).filter(el => !el.hasAttribute('disabled'))
        if (tabbable.length === 0) return
        const first = tabbable[0]
        const last = tabbable[tabbable.length - 1]
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

  // Focus lifecycle, keyed on isOpen ALONE. It must NOT depend on handleKeyDown:
  // a parent that passes an inline onClose hands us a new identity every render,
  // and folding that into this effect would tear down and re-run the capture /
  // restore pair mid-interaction — yanking focus back out of the dialog.
  useEffect(() => {
    if (!isOpen) return
    previousFocusRef.current = document.activeElement as HTMLElement
    const first = containerRef.current?.querySelector<HTMLElement>(FOCUSABLE)
    const raf = requestAnimationFrame(() => first?.focus())
    return () => {
      cancelAnimationFrame(raf)
      // Restore from the CLEANUP rather than an `else` branch on isOpen. The
      // prevailing call pattern is `{open && <Dialog isOpen … />}`, which
      // UNMOUNTS while isOpen is still true — an else-branch restore never runs
      // there and focus is silently dropped onto <body>.
      previousFocusRef.current?.focus()
      previousFocusRef.current = null
    }
  }, [isOpen, containerRef])

  useEffect(() => {
    if (!isOpen) return
    document.addEventListener('keydown', handleKeyDown)
    return () => document.removeEventListener('keydown', handleKeyDown)
  }, [isOpen, handleKeyDown])
}
