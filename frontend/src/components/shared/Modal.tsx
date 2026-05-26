import {useRef, useEffect, useCallback} from 'react'
import clsx from 'clsx'

type ModalProps = {
    isOpen: boolean
    onClose: () => void
    title?: string
    size?: 'sm' | 'md' | 'lg' | 'xl'
    closeOnBackdrop?: boolean
    closeOnEsc?: boolean
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
    size = 'md',
    closeOnBackdrop = true,
    closeOnEsc = true,
    children,
    footer,
}: ModalProps) {
    const modalRef = useRef<HTMLDivElement>(null)
    const previousFocusRef = useRef<HTMLElement | null>(null)

    const handleKeyDown = useCallback((e: KeyboardEvent) => {
        if (closeOnEsc && e.key === 'Escape') {
            onClose()
            return
        }
        if (e.key === 'Tab' && modalRef.current) {
            const focusable = modalRef.current.querySelectorAll<HTMLElement>(
                'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
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
    }, [closeOnEsc, onClose])

    useEffect(() => {
        if (isOpen) {
            previousFocusRef.current = document.activeElement as HTMLElement
            document.body.style.overflow = 'hidden'
            document.addEventListener('keydown', handleKeyDown)
            const firstFocusable = modalRef.current?.querySelector<HTMLElement>(
                'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
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

    return (
        <div className="fixed inset-0 z-modal flex items-center justify-center">
            <div
                className="absolute inset-0 bg-surface-overlay animate-fade-in"
                onClick={closeOnBackdrop ? onClose : undefined}
            />
            <div
                ref={modalRef}
                className={clsx(
                    'relative w-full mx-4 bg-surface-1 border border-border-default rounded-xl shadow-xl animate-modal',
                    sizeClasses[size]
                )}
                role="dialog"
                aria-modal="true"
                aria-label={title}
            >
                {title && (
                    <div className="flex items-center justify-between px-6 py-4 border-b border-border-subtle">
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
                <div className="px-6 py-4 overflow-y-auto max-h-[70vh]">
                    {children}
                </div>
                {footer && (
                    <div className="flex items-center justify-end gap-3 px-6 py-4 border-t border-border-subtle">
                        {footer}
                    </div>
                )}
            </div>
        </div>
    )
}
