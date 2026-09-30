import {useTranslation} from 'react-i18next'
import React, {useState, useCallback, useRef, useMemo, useId} from 'react'
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

type DialogState = ({kind: 'confirm'} & ConfirmOptions) | ({kind: 'prompt'} & PromptOptions)

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
 *
 * Because the heading is our own, the dialog is named and described by id:
 * aria-labelledby points at the heading and aria-describedby at the message.
 * Without them every confirm announced as an unnamed "dialog".
 */
export function ConfirmProvider({children}: {children: React.ReactNode}) {
  const {t} = useTranslation()
  const [state, setState] = useState<DialogState | null>(null)
  const [inputValue, setInputValue] = useState('')
  const resolverRef = useRef<((value: boolean | string | null) => void) | null>(null)
  const titleId = useId()
  const messageId = useId()
  const inputId = useId()

  const settle = useCallback((value: boolean | string | null) => {
    resolverRef.current?.(value)
    resolverRef.current = null
    setState(null)
  }, [])

  const confirm = useCallback(
    (opts: ConfirmOptions) =>
      new Promise<boolean>(resolve => {
        resolverRef.current = resolve as (v: boolean | string | null) => void
        setState({kind: 'confirm', ...opts})
      }),
    [],
  )

  const prompt = useCallback(
    (opts: PromptOptions) =>
      new Promise<string | null>(resolve => {
        resolverRef.current = resolve as (v: boolean | string | null) => void
        setInputValue(opts.initialValue ?? '')
        setState({kind: 'prompt', ...opts})
      }),
    [],
  )

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

  const confirmLabel = state?.confirmLabel ?? (danger ? t('delete') : isPrompt ? t('save') : t('confirm'))
  const cancelLabel = state?.cancelLabel ?? t('cancel')

  // Named by the heading, described by the message. Without a title the
  // message is the name (and not repeated as the description); a bare
  // prompt falls back to its field label.
  const hasTitle = !!state?.title
  const hasMessage = !!state?.message
  const promptLabel = state?.kind === 'prompt' ? state.label : undefined
  const labelledBy = hasTitle ? titleId : hasMessage ? messageId : promptLabel ? `${inputId}-label` : undefined

  return (
    <ConfirmContext.Provider value={actions}>
      {children}
      <Modal
        isOpen={state !== null}
        onClose={handleCancel}
        size="sm"
        labelledBy={labelledBy}
        describedBy={hasTitle && hasMessage ? messageId : undefined}
        footer={
          state ? (
            <>
              <button
                type="button"
                onClick={handleCancel}
                className="px-3 py-1.5 rounded-lg text-sm text-text-secondary hover:bg-surface-3 transition-colors"
              >
                {cancelLabel}
              </button>
              <button
                type="button"
                onClick={handleConfirm}
                disabled={!canConfirm}
                className={clsx(
                  'px-3 py-1.5 rounded-lg text-sm font-medium transition-colors disabled:opacity-50',
                  // The theme's error colour with its own foreground token
                  // (≥ 4.5:1 in every theme, themeContrast.test). The raw
                  // red-600 ignored the theme; hover rings the button rather
                  // than shading it, so the text never loses contrast.
                  danger
                    ? 'bg-semantic-error text-[color:var(--error-foreground)] hover:ring-2 hover:ring-semantic-error/40'
                    : 'bg-brand-600 hover:bg-brand-700 text-brand-foreground',
                )}
              >
                {confirmLabel}
              </button>
            </>
          ) : undefined
        }
      >
        {state && (
          <div className="space-y-3">
            {state.title && (
              <h2 id={titleId} className="text-base font-semibold text-text-primary">
                {state.title}
              </h2>
            )}
            {state.kind === 'confirm' ? (
              <div id={messageId} className="text-sm text-text-secondary">
                {state.message}
              </div>
            ) : (
              <div className="space-y-2">
                {state.message && (
                  <div id={messageId} className="text-sm text-text-secondary">
                    {state.message}
                  </div>
                )}
                {state.label && (
                  <label
                    id={`${inputId}-label`}
                    htmlFor={inputId}
                    className="block text-sm font-medium text-text-primary"
                  >
                    {state.label}
                  </label>
                )}
                <input
                  id={inputId}
                  autoFocus
                  value={inputValue}
                  onChange={e => setInputValue(e.target.value)}
                  onKeyDown={e => {
                    if (e.key === 'Enter') {
                      e.preventDefault()
                      handleConfirm()
                    }
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
