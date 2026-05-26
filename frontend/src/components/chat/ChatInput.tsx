import clsx from 'clsx'
import {Square, Eye, ArrowUp} from 'lucide-react'
import {useRef, useEffect, useCallback, useState} from 'react'
import {useChatStore} from '@/stores/chatStore'

interface Props {
  onSend: (text: string) => void
  onPreview?: (text: string) => void
  onCancel?: () => void
  disabled?: boolean
  placeholder?: string
}

export default function ChatInput({onSend, onPreview, onCancel, disabled, placeholder}: Props) {
  const [value, setValue] = useState('')
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const streaming = useChatStore(s => s.activeStreamId !== null)

  const isDisabled = disabled || streaming

  const adjustHeight = useCallback(() => {
    const el = textareaRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = Math.min(el.scrollHeight, 140) + 'px'
  }, [])

  useEffect(() => {
    adjustHeight()
  }, [value, adjustHeight])

  useEffect(() => {
    if (!streaming && !disabled) {
      textareaRef.current?.focus()
    }
  }, [streaming, disabled])

  const handleSend = useCallback(() => {
    const trimmed = value.trim()
    if (!trimmed || isDisabled) return
    onSend(trimmed)
    setValue('')
    requestAnimationFrame(() => {
      if (textareaRef.current) {
        textareaRef.current.style.height = 'auto'
      }
    })
  }, [value, isDisabled, onSend])

  const handlePreview = useCallback(() => {
    const trimmed = value.trim()
    if (!trimmed || isDisabled || !onPreview) return
    onPreview(trimmed)
  }, [value, isDisabled, onPreview])

  const handleKeyDown = useCallback((e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.metaKey && !e.ctrlKey) {
      e.preventDefault()
      handleSend()
      return
    }
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      if (e.shiftKey && onPreview) {
        handlePreview()
      } else {
        handleSend()
      }
    }
  }, [handleSend, handlePreview, onPreview])

  const hasContent = value.trim().length > 0

  return (
    <div className="px-3 pb-3 pt-1">
      <div
        className={clsx(
          'flex items-end gap-2 bg-surface-2 border rounded-xl px-3 py-2 transition-colors',
          hasContent && !isDisabled
            ? 'border-brand-500/40 ring-1 ring-brand-500/10'
            : 'border-border-default'
        )}
      >
        <textarea
          ref={textareaRef}
          className="flex-1 bg-transparent text-sm resize-none focus:outline-none min-h-[20px] max-h-[140px] placeholder:text-text-tertiary/60 leading-relaxed"
          rows={1}
          value={value}
          onChange={e => setValue(e.target.value)}
          onKeyDown={handleKeyDown}
          placeholder={placeholder || 'Ask about your flow\u2026'}
          disabled={isDisabled && !streaming}
        />
        {streaming ? (
          <button
            className="p-1.5 rounded-lg bg-red-500/15 hover:bg-red-500/25 text-red-400 transition-colors shrink-0"
            onClick={onCancel}
            aria-label="Stop generating"
            title="Stop generating"
          >
            <Square size={14} />
          </button>
        ) : (
          <div className="flex items-center gap-1 shrink-0">
            {onPreview && hasContent && (
              <button
                className="p-1.5 rounded-lg hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors"
                onClick={handlePreview}
                aria-label="Preview context"
                title="Preview context (Ctrl+Shift+Enter)"
              >
                <Eye size={14} />
              </button>
            )}
            <button
              className={clsx(
                'p-1.5 rounded-lg transition-all',
                hasContent && !isDisabled
                  ? 'bg-brand-500 hover:bg-brand-600 text-white shadow-sm shadow-brand-500/20'
                  : 'bg-surface-3 text-text-tertiary'
              )}
              onClick={handleSend}
              disabled={!hasContent || isDisabled}
              aria-label="Send message"
            >
              <ArrowUp size={14} strokeWidth={2.5} />
            </button>
          </div>
        )}
      </div>
      <div className="flex justify-between mt-1.5 px-0.5">
        <span className="text-2xs text-text-tertiary/50">
          Enter to send · Shift+Enter for newline
        </span>
        {onPreview && (
          <span className="text-2xs text-text-tertiary/50">
            {navigator.platform?.includes('Mac') ? '\u2318' : 'Ctrl'}+Shift+Enter preview
          </span>
        )}
      </div>
    </div>
  )
}
