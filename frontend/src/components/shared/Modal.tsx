import {useRef, useEffect, useCallback} from 'react'
import clsx from 'clsx'
import Portal from './Portal'

type ModalProps = {
  isOpen: boolean
  onClose: () => void
  title?: string
  // Accessible name when title isn't rendered (title bar hidden). Provide
  // title OR ariaLabel so screen readers always announce the dialog.
  ariaLabel?: string
  size?: 'sm' | 'md' | 'lg' | 'xl'
  closeOnBackdrop?: boolean
  closeOnEsc?: boolean
  // 'tall' makes the panel a fixed-height flex column so its size stays
  // constant regardless of content (avoids the window resizing per tab).
  // Defaults to 'auto' (height driven by content, capped at 70vh).
  height?: 'auto' | 'tall'
  // When false, the body wrapper does not scroll or pad — the child owns the
  // full area and provides its own single scroll region. Defaults to true.
  bodyScroll?: boolean
  children: React.ReactNode
  footer?: React.ReactNode
}

const sizeClasses: Record<string, string> = {
  sm: 'max-w-sm',
  md: 'max-w-lg',
  lg: 'max-w-2xl',
  xl: 'max-w-4xl',
}

export default function Modal({
  isOpen,
  onClose,
  title,
  ariaLabel,
  size = 'md',
  closeOnBackdrop = true,
  closeOnEsc = true,
  height = 'auto',
  bodyScroll = true,
  children,
  footer,
}: ModalProps) {
  const modalRef = useRef<HTMLDivElement>(null)
  const previousFocusRef = useRef<HTMLElement | null>(null)

  const handleKeyDown = useCallback(
    (e: KeyboardEvent) => {
      if (closeOnEsc && e.key === 'Escape') {
        onClose()
        return
      }
      if (e.key === 'Tab' && modalRef.current) {
        const focusable = modalRef.current.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
        )
        const first = focusable[0]
        const last = focusable[focusable.length - 1]
        if (e.shiftKey && document.activeElement === first) {
          e.preventDefault()
          last?.focus()
        } else if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault()
          first?.focus()
        }
      }
    },
    [closeOnEsc, onClose],
  )

  useEffect(() => {
    if (isOpen) {
      previousFocusRef.current = document.activeElement as HTMLElement
      document.body.style.overflow = 'hidden'
      document.addEventListener('keydown', handleKeyDown)
      const firstFocusable = modalRef.current?.querySelector<HTMLElement>(
        'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
      )
      requestAnimationFrame(() => firstFocusable?.focus())
    } else {
      document.body.style.overflow = ''
      document.removeEventListener('keydown', handleKeyDown)
      previousFocusRef.current?.focus()
    }
    return () => {
      document.body.style.overflow = ''
      document.removeEventListener('keydown', handleKeyDown)
    }
  }, [isOpen, handleKeyDown])

  if (!isOpen) return null

  // Render through a portal to document.body so the dialog escapes any
  // ancestor that creates a stacking context (transform/filter/perspective)
  // or clips with overflow:hidden — both would defeat z-index / clip a
  // fixed-position overlay.
  return (
    <Portal>
      <div className="fixed inset-0 z-modal flex items-center justify-center">
        <div
          className="absolute inset-0 bg-surface-overlay animate-fade-in"
          onClick={closeOnBackdrop ? onClose : undefined}
        />
        <div
          ref={modalRef}
          className={clsx(
            'relative w-full mx-4 bg-surface-1 border border-border-default rounded-xl shadow-xl animate-modal',
            sizeClasses[size],
            height === 'tall' && 'flex flex-col h-[80vh] max-h-[820px]',
          )}
          role="dialog"
          aria-modal="true"
          aria-label={title ?? ariaLabel}
        >
          {title && (
            <div className="flex items-center justify-between px-6 py-4 border-b border-border-subtle shrink-0">
              <h2 className="text-lg font-semibold text-text-primary">{title}</h2>
              <button
                onClick={onClose}
                className="w-7 h-7 flex items-center justify-center rounded-md text-text-tertiary hover:text-text-primary hover:bg-surface-2 transition-colors duration-fast"
                aria-label="Close"
              >
                ✕
              </button>
            </div>
          )}
          <div
            className={clsx(
              bodyScroll
                ? height === 'tall'
                  ? 'px-6 py-4 overflow-y-auto flex-1 min-h-0'
                  : 'px-6 py-4 overflow-y-auto max-h-[70vh]'
                : 'flex-1 min-h-0 overflow-hidden',
            )}
          >
            {children}
          </div>
          {footer && (
            <div className="flex items-center justify-end gap-3 px-6 py-4 border-t border-border-subtle shrink-0">
              {footer}
            </div>
          )}
        </div>
      </div>
    </Portal>
  )
}
