import React, {useState, useCallback, useRef, useEffect, useMemo} from 'react'
import clsx from 'clsx'

type ToastData = {
    id: string
    variant?: 'info' | 'success' | 'warning' | 'error'
    title: string
    description?: string
    action?: {label: string; onClick: () => void}
    duration?: number
}

type ToastProps = ToastData & {
    onClose: (id: string) => void
}

const variantColors: Record<string, string> = {
    info: 'border-semantic-info',
    success: 'border-semantic-success',
    warning: 'border-semantic-warning',
    error: 'border-semantic-error',
}

const variantIcons: Record<string, string> = {
    info: 'ℹ',
    success: '✓',
    warning: '⚠',
    error: '✕',
}

function Toast({id, variant = 'info', title, description, action, duration = 4000, onClose}: ToastProps) {
    const [exiting, setExiting] = useState(false)

    const handleClose = useCallback(() => {
        setExiting(true)
        setTimeout(() => onClose(id), 200)
    }, [id, onClose])

    useEffect(() => {
        if (duration > 0) {
            const timer = setTimeout(handleClose, duration)
            return () => clearTimeout(timer)
        }
    }, [duration, handleClose])

    return (
        <div
            className={clsx(
                'w-[360px] max-w-[calc(100vw-2rem)] bg-surface-2 border border-border-default shadow-lg rounded-lg p-3 flex gap-3 border-l-4',
                variantColors[variant],
                exiting ? 'animate-toast-out' : 'animate-toast-in'
            )}
            role="alert"
        >
            <span className="text-base flex-shrink-0">{variantIcons[variant]}</span>
            <div className="flex-1 min-w-0">
                <div className="text-sm font-medium text-text-primary">{title}</div>
                {description && (
                    <div className="text-xs text-text-secondary mt-0.5">{description}</div>
                )}
                {action && (
                    <button
                        onClick={action.onClick}
                        className="text-xs text-brand-400 hover:text-brand-300 mt-1 font-medium"
                    >
                        {action.label}
                    </button>
                )}
            </div>
            <button
                onClick={handleClose}
                className="text-text-tertiary hover:text-text-secondary flex-shrink-0 text-sm"
                aria-label="Dismiss"
            >
                ✕
            </button>
        </div>
    )
}

type ToastActions = {
    addToast: (toast: Omit<ToastData, 'id'>) => void
    removeToast: (id: string) => void
}

// Split into two contexts so useToast() consumers don't re-render when
// toasts are added/removed — only the actions are stable; the toast list
// is consumed by ToastList below.
const ToastActionsContext = React.createContext<ToastActions | null>(null)
const ToastListContext = React.createContext<ToastData[]>([])

function ToastList() {
    const toasts = React.useContext(ToastListContext)
    const actions = React.useContext(ToastActionsContext)
    if (!actions) return null
    const removeToast = actions.removeToast
    return (
        <div className="fixed bottom-4 right-4 z-toast flex flex-col-reverse gap-2">
            {toasts.map(toast => (
                <Toast key={toast.id} {...toast} onClose={removeToast} />
            ))}
        </div>
    )
}

export function ToastProvider({children}: {children: React.ReactNode}) {
    const [toasts, setToasts] = useState<ToastData[]>([])
    const counterRef = useRef(0)

    const addToast = useCallback((toast: Omit<ToastData, 'id'>) => {
        const id = `toast-${++counterRef.current}`
        setToasts(prev => [...prev, {...toast, id}])
    }, [])

    const removeToast = useCallback((id: string) => {
        setToasts(prev => prev.filter(t => t.id !== id))
    }, [])

    const actions = useMemo(() => ({addToast, removeToast}), [addToast, removeToast])

    return (
        <ToastActionsContext.Provider value={actions}>
            <ToastListContext.Provider value={toasts}>
                {children}
                <ToastList />
            </ToastListContext.Provider>
        </ToastActionsContext.Provider>
    )
}

export function useToast() {
    const ctx = React.useContext(ToastActionsContext)
    if (!ctx) throw new Error('useToast must be used within ToastProvider')
    return useMemo(() => ({
        toast: ctx.addToast,
        success: (title: string, opts?: Partial<ToastData>) => ctx.addToast({variant: 'success', title, ...opts}),
        error: (title: string, opts?: Partial<ToastData>) => ctx.addToast({variant: 'error', title, ...opts}),
        warning: (title: string, opts?: Partial<ToastData>) => ctx.addToast({variant: 'warning', title, ...opts}),
        info: (title: string, opts?: Partial<ToastData>) => ctx.addToast({variant: 'info', title, ...opts}),
    }), [ctx.addToast])
}
