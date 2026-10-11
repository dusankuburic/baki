import {useTranslation} from 'react-i18next'
import {useRef, useEffect, useId} from 'react'
import clsx from 'clsx'
import {X} from 'lucide-react'
import Portal from './Portal'
import {useDialogFocus} from '@/hooks/useDialogFocus'

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
  ariaLabel?: string
  labelledBy?: string
  description?: React.ReactNode
  describedBy?: string
  size?: 'sm' | 'md' | 'lg' | 'xl' | '2xl'
  fullScreenBelow?: 'sm'
  closeOnBackdrop?: boolean
  closeOnEsc?: boolean
  height?: 'auto' | 'tall' | 'fill'
  className?: string
  bodyScroll?: boolean
  initialFocusRef?: React.RefObject<HTMLElement | null>
  viewTransitionName?: string
  children: React.ReactNode
  footer?: React.ReactNode
}

const sizeClasses: Record<string, string> = {
  sm: 'max-w-sm',
  md: 'max-w-lg',
  lg: 'max-w-2xl',
  xl: 'max-w-4xl',
  '2xl': 'max-w-300',
}

const fullScreenPanelClasses: Record<'sm', string> = {
  sm: 'max-sm:mx-0 max-sm:h-full max-sm:max-h-none max-sm:max-w-none max-sm:rounded-none max-sm:border-0 max-sm:flex max-sm:flex-col',
}
const fullScreenBodyClasses: Record<'sm', string> = {
  sm: 'max-sm:max-h-none max-sm:flex-1 max-sm:min-h-0',
}

export default function Modal({
  isOpen,
  onClose,
  title,
  ariaLabel,
  labelledBy,
  description,
  describedBy,
  size = 'md',
  fullScreenBelow,
  closeOnBackdrop = true,
  closeOnEsc = true,
  height = 'auto',
  bodyScroll = true,
  initialFocusRef,
  viewTransitionName,
  className,
  children,
  footer,
}: ModalProps) {
  const {t} = useTranslation()
  const modalRef = useRef<HTMLDivElement>(null)
  const titleId = useId()
  const descriptionId = useId()
  const describedByIds = [describedBy, description ? descriptionId : undefined].filter(Boolean).join(' ')

  useDialogFocus({isOpen, onClose, closeOnEsc, containerRef: modalRef, initialFocusRef})

  useEffect(() => {
    if (!isOpen) return
    acquireScrollLock()
    return releaseScrollLock
  }, [isOpen])

  if (!isOpen) return null

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
            height === 'fill' && 'flex flex-col h-[min(60rem,calc(100vh-3rem))]',
            fullScreenBelow && fullScreenPanelClasses[fullScreenBelow],
            className,
          )}
          style={viewTransitionName ? {viewTransitionName} : undefined}
          role="dialog"
          aria-modal="true"
          aria-labelledby={labelledBy ?? (title ? titleId : undefined)}
          aria-label={labelledBy || title ? undefined : ariaLabel}
          aria-describedby={describedByIds || undefined}
        >
          {title && (
            <div
              className={clsx(
                'flex justify-between px-6 py-4 border-b border-border-subtle shrink-0',
                description ? 'items-start gap-4' : 'items-center',
              )}
            >
              <div className="min-w-0">
                <h2 id={titleId} className="text-lg font-semibold text-text-primary">
                  {title}
                </h2>
                {description && (
                  <div id={descriptionId} className="mt-1 text-sm text-text-secondary">
                    {description}
                  </div>
                )}
              </div>
              <button
                type="button"
                onClick={onClose}
                className="w-7 h-7 shrink-0 flex items-center justify-center rounded-md text-text-tertiary hover:text-text-primary hover:bg-surface-2 transition-colors duration-fast"
                aria-label={t('close')}
              >
                <X size={16} aria-hidden="true" />
              </button>
            </div>
          )}
          {!title && description && (
            <div id={descriptionId} className="px-6 pt-4 text-sm text-text-secondary shrink-0">
              {description}
            </div>
          )}
          <div
            className={clsx(
              bodyScroll
                ? height !== 'auto'
                  ? 'px-6 py-4 overflow-y-auto flex-1 min-h-0'
                  : 'px-6 py-4 overflow-y-auto max-h-[70vh]'
                : 'flex-1 min-h-0 overflow-hidden',
              fullScreenBelow && fullScreenBodyClasses[fullScreenBelow],
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
