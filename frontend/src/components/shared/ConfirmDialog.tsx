import React, {useState, useCallback, useRef, useMemo} from 'react'
import clsx from 'clsx'
import Modal from './Modal'

type ConfirmOptions = {
    title?: string
    message: React.ReactNode
    confirmLabel?: string
    cancelLabel?: string
    danger?: boolean
}

type PromptOptions = {
    title?: string
    label?: React.ReactNode
    message?: React.ReactNode
    initialValue?: string
    placeholder?: string
    confirmLabel?: string
    cancelLabel?: string
}

type DialogState =
    | ({kind: 'confirm'} & ConfirmOptions)
    | ({kind: 'prompt'} & PromptOptions)

type ConfirmActions = {
    confirm: (opts: ConfirmOptions) => Promise<boolean>
    prompt: (opts: PromptOptions) => Promise<string | null>
}

const ConfirmContext = React.createContext<ConfirmActions | null>(null)

/**
 * Promise-based replacement for the native window.confirm()/prompt() dialogs.
 * `confirm()` resolves a boolean; `prompt()` resolves the entered string or null
 * on cancel. Built on the shared Modal (focus-trap + Esc-to-close). We render our
 * own heading inside the body rather than using Modal's `title` prop so the first
 * focusable element is the input (for prompts) or the Cancel button (for
 * destructive confirms) — never the destructive Confirm action.
 */
export function ConfirmProvider({children}: {children: React.ReactNode}) {
    const [state, setState] = useState<DialogState | null>(null)
    const [inputValue, setInputValue] = useState('')
    const resolverRef = useRef<((value: boolean | string | null) => void) | null>(null)

    const settle = useCallback((value: boolean | string | null) => {
        resolverRef.current?.(value)
        resolverRef.current = null
        setState(null)
    }, [])

    const confirm = useCallback((opts: ConfirmOptions) =>
        new Promise<boolean>(resolve => {
            resolverRef.current = resolve as (v: boolean | string | null) => void
            setState({kind: 'confirm', ...opts})
        }), [])

    const prompt = useCallback((opts: PromptOptions) =>
        new Promise<string | null>(resolve => {
            resolverRef.current = resolve as (v: boolean | string | null) => void
            setInputValue(opts.initialValue ?? '')
            setState({kind: 'prompt', ...opts})
        }), [])

    const actions = useMemo(() => ({confirm, prompt}), [confirm, prompt])

    const isPrompt = state?.kind === 'prompt'
    const danger = state?.kind === 'confirm' && state.danger
    const canConfirm = !isPrompt || inputValue.trim().length > 0

    const handleCancel = useCallback(() => {
        settle(isPrompt ? null : false)
    }, [settle, isPrompt])

    const handleConfirm = useCallback(() => {
        if (!canConfirm) return
        settle(isPrompt ? inputValue.trim() : true)
    }, [settle, isPrompt, inputValue, canConfirm])

    const confirmLabel = state?.confirmLabel ?? (danger ? 'Delete' : isPrompt ? 'Save' : 'Confirm')
    const cancelLabel = state?.cancelLabel ?? 'Cancel'

    return (
        <ConfirmContext.Provider value={actions}>
            {children}
            <Modal
                isOpen={state !== null}
                onClose={handleCancel}
                size="sm"
                footer={state ? (
                    <>
                        <button
                            onClick={handleCancel}
                            className="px-3 py-1.5 rounded-lg text-sm text-text-secondary hover:bg-surface-3 transition-colors"
                        >
                            {cancelLabel}
                        </button>
                        <button
                            onClick={handleConfirm}
                            disabled={!canConfirm}
                            className={clsx(
                                'px-3 py-1.5 rounded-lg text-sm font-medium text-brand-foreground transition-colors disabled:opacity-50',
                                danger ? 'bg-red-600 hover:bg-red-700' : 'bg-brand-600 hover:bg-brand-700',
                            )}
                        >
                            {confirmLabel}
                        </button>
                    </>
                ) : undefined}
            >
                {state && (
                    <div className="space-y-3">
                        {state.title && (
                            <h2 className="text-base font-semibold text-text-primary">{state.title}</h2>
                        )}
                        {state.kind === 'confirm' ? (
                            <div className="text-sm text-text-secondary">{state.message}</div>
                        ) : (
                            <div className="space-y-2">
                                {state.message && <div className="text-sm text-text-secondary">{state.message}</div>}
                                {state.label && (
                                    <label className="block text-sm font-medium text-text-primary">{state.label}</label>
                                )}
                                <input
                                    autoFocus
                                    value={inputValue}
                                    onChange={e => setInputValue(e.target.value)}
                                    onKeyDown={e => {
                                        if (e.key === 'Enter') { e.preventDefault(); handleConfirm() }
                                    }}
                                    placeholder={state.placeholder}
                                    className="w-full px-3 py-2 rounded-lg bg-surface-2 border border-border-default text-sm text-text-primary focus:outline-none focus:border-brand-500"
                                />
                            </div>
                        )}
                    </div>
                )}
            </Modal>
        </ConfirmContext.Provider>
    )
}

export function useConfirm() {
    const ctx = React.useContext(ConfirmContext)
    if (!ctx) throw new Error('useConfirm must be used within ConfirmProvider')
    return ctx
}
