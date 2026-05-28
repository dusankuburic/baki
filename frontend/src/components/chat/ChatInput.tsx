import clsx from 'clsx'
import {Square, Eye, ArrowUp} from 'lucide-react'
import {useRef, useEffect, useCallback, useState} from 'react'
import {useChatStore} from '@/stores/chatStore'
import FileAutocomplete from './FileAutocomplete'

interface Props {
  onSend: (text: string, files: string[], excludeContext?: boolean) => void
  onPreview?: (text: string, files: string[], excludeContext?: boolean) => void
  onCancel?: () => void
  onFilesChange?: (files: string[]) => void
  disabled?: boolean
  placeholder?: string
}

export default function ChatInput({onSend, onPreview, onCancel, onFilesChange, disabled, placeholder}: Props) {
  const [value, setValue] = useState('')
  const [autocompleteQuery, setAutocompleteQuery] = useState<string | null>(null)
  const [taggedFiles, setTaggedFiles] = useState<string[]>([])
  const [excludeContext, setExcludeContext] = useState(false)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const streaming = useChatStore(s => s.activeStreamId !== null)

  const isDisabled = disabled || streaming

  const extractFiles = useCallback((text: string) => {
    const regex = /@([a-zA-Z0-9_./-]+)/g
    return Array.from(new Set(Array.from(text.matchAll(regex), m => m[1])))
  }, [])

  useEffect(() => {
    const files = extractFiles(value)
    if (JSON.stringify(files) !== JSON.stringify(taggedFiles)) {
      setTaggedFiles(files)
      onFilesChange?.(files)
    }
  }, [value, taggedFiles, onFilesChange, extractFiles])

  const adjustHeight = useCallback(() => {
    const el = textareaRef.current
    if (!el) return

    const currentHeight = parseInt(el.style.height || '0', 10)
    const newHeight = Math.min(el.scrollHeight, 140)

    // Only animate significant changes to avoid jitter
    if (Math.abs(currentHeight - newHeight) > 4) {
      el.style.transition = 'height 200ms var(--ease-spring)'
    } else {
      el.style.transition = 'none'
    }

    el.style.height = 'auto'
    el.style.height = newHeight + 'px'

    // Reset transition after animation completes
    setTimeout(() => {
      el.style.transition = ''
    }, 200)
  }, [])

  useEffect(() => {
    adjustHeight()
  }, [value, adjustHeight])

  const handleSend = useCallback(() => {
    const trimmed = value.trim()
    if (!trimmed || isDisabled) return
    onSend(trimmed, extractFiles(trimmed), excludeContext)
    setValue('')
    setTaggedFiles([])
    setAutocompleteQuery(null)
    requestAnimationFrame(() => {
      if (textareaRef.current) {
        textareaRef.current.style.height = 'auto'
      }
    })
  }, [value, isDisabled, onSend, extractFiles, excludeContext])

  const handlePreview = useCallback(() => {
    const trimmed = value.trim()
    if (!trimmed || isDisabled || !onPreview) return
    onPreview(trimmed, extractFiles(trimmed), excludeContext)
  }, [value, isDisabled, onPreview, excludeContext])

  const handleKeyDown = useCallback((e: React.KeyboardEvent) => {
    if (autocompleteQuery !== null) {
      if (e.key === 'Escape') {
        setAutocompleteQuery(null)
        e.preventDefault()
      }
      return
    }

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
  }, [handleSend, handlePreview, onPreview, autocompleteQuery])

  const handleChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const val = e.target.value
    setValue(val)

    const pos = e.target.selectionStart
    const textBefore = val.slice(0, pos)
    const atIdx = textBefore.lastIndexOf('@')
    
    if (atIdx !== -1) {
      const query = textBefore.slice(atIdx + 1)
      if (!query.includes(' ')) {
        setAutocompleteQuery(query)
      } else {
        setAutocompleteQuery(null)
      }
    } else {
      setAutocompleteQuery(null)
    }
  }

  const handleSelectFile = (filename: string) => {
    const pos = textareaRef.current?.selectionStart ?? value.length
    const textBefore = value.slice(0, pos)
    const atIdx = textBefore.lastIndexOf('@')
    const newValue = value.slice(0, atIdx + 1) + filename + ' ' + value.slice(pos)
    setValue(newValue)
    setAutocompleteQuery(null)
    textareaRef.current?.focus()
  }

  const hasContent = value.trim().length > 0

  return (
    <div className="px-3 pb-3 pt-1">
      <div
        className={clsx(
          'relative flex items-end gap-2 bg-surface-2 border rounded-xl px-3 py-2 transition-all duration-200',
          hasContent && !isDisabled
            ? 'border-brand-500/40 ring-1 ring-brand-500/10 shadow-lg shadow-brand-500/5'
            : 'border-border-default hover:border-border-strong'
        )}
      >
        {autocompleteQuery !== null && (
          <FileAutocomplete
            query={autocompleteQuery}
            onSelect={handleSelectFile}
            onClose={() => setAutocompleteQuery(null)}
          />
        )}
        <textarea
          ref={textareaRef}
          className="flex-1 bg-transparent text-sm resize-none focus:outline-none min-h-[20px] max-h-[140px] placeholder:text-text-tertiary/60 leading-relaxed no-scrollbar"
          rows={1}
          value={value}
          onChange={handleChange}
          onKeyDown={handleKeyDown}
          placeholder={placeholder || 'Ask about your flow, use @ to tag files\u2026'}
          disabled={isDisabled && !streaming}
        />
        {streaming ? (
          <button
            className="p-1.5 rounded-lg bg-red-500 hover:bg-red-600 text-white transition-all duration-150 animate-pulse-soft shrink-0"
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
                'p-1.5 rounded-lg transition-all duration-150',
                hasContent && !isDisabled
                  ? 'bg-brand-500 hover:bg-brand-600 text-white shadow-sm shadow-brand-500/20 animate-btn-pulse'
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
      <div className="flex justify-between items-center mt-1.5 px-0.5">
        <label className="flex items-center gap-1.5 cursor-pointer group">
          <input
            type="checkbox"
            checked={excludeContext}
            onChange={(e) => setExcludeContext(e.target.checked)}
            className="w-3 h-3 rounded border-border-subtle text-brand-500 focus:ring-brand-500 focus:ring-offset-0"
            disabled={isDisabled}
          />
          <span className="text-2xs text-text-tertiary group-hover:text-text-secondary transition-colors">
            Send without context
          </span>
        </label>
        <div className="flex gap-2">
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
    </div>
  )
}
