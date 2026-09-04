import {useTranslation} from 'react-i18next'
import {useRef, useEffect} from 'react'
import clsx from 'clsx'
import Portal from './Portal'
import {useDialogFocus} from '@/hooks/useDialogFocus'

// Body-scroll lock is REFCOUNTED across every Modal instance. Modals nest
// (Settings → a confirm dialog inside it), and the previous per-instance
// cleanup reset body overflow unconditionally — so tearing down the INNER
// modal unlocked scrolling behind the outer one that was still open.
let scrollLockCount = 0
let previousBodyOverflow = ''

function acquireScrollLock(): void {
  if (scrollLockCount === 0) {
    previousBodyOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
  }
  scrollLockCount++
}

function releaseScrollLock(): void {
  if (scrollLockCount === 0) return
  scrollLockCount--
  if (scrollLockCount === 0) document.body.style.overflow = previousBodyOverflow
}

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
  const {t} = useTranslation()
  const modalRef = useRef<HTMLDivElement>(null)

  // Focus trap, Esc-to-close and focus restoration come from the shared hook —
  // <Modal> used to carry a byte-identical private copy of all three.
  useDialogFocus({isOpen, onClose, closeOnEsc, containerRef: modalRef})

  useEffect(() => {
    if (!isOpen) return
    acquireScrollLock()
    return releaseScrollLock
  }, [isOpen])

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
                aria-label={t('close')}
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
