import {useRef, useEffect} from 'react'
import {X, Send, Eye} from 'lucide-react'
import {createPortal} from 'react-dom'

interface Props {
  value: string
  onChange: (val: string) => void
  onSend: () => void
  onPreview: () => void
  onClose: () => void
  excludeContext: boolean
  onExcludeContextChange: (val: boolean) => void
}

export default function ExpandedChatInput({
  value,
  onChange,
  onSend,
  onPreview,
  onClose,
  excludeContext,
  onExcludeContextChange,
}: Props) {
  const containerRef = useRef<HTMLDivElement>(null)

  // Close on Escape
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !e.shiftKey) {
        onClose()
      }
    }
    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [onClose])

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm animate-in fade-in duration-200 p-4 sm:p-8">
      <div
        ref={containerRef}
        className="w-full max-w-4xl bg-surface-1 border border-border-default rounded-2xl shadow-2xl flex flex-col overflow-hidden animate-in zoom-in-95 duration-200 h-[80vh]"
      >
        {/* Header */}
        <div className="flex items-center justify-between px-6 py-4 border-b border-border-subtle bg-surface-2">
          <div className="flex flex-col">
            <h3 className="text-lg font-semibold text-text-primary">Compose Prompt</h3>
            <p className="text-xs text-text-tertiary">
              Write a detailed prompt. Press Enter to send, Shift+Enter for new line.
            </p>
          </div>
          <button
            onClick={onClose}
            className="p-2 rounded-full hover:bg-surface-3 text-text-tertiary hover:text-text-primary transition-colors"
            aria-label="Close"
          >
            <X size={20} />
          </button>
        </div>

        {/* Editor Area */}
        <div className="flex-1 relative flex flex-col p-6 bg-surface-1">
          <textarea
            autoFocus
            className="flex-1 w-full bg-transparent text-text-primary placeholder:text-text-tertiary resize-none focus:outline-none text-base leading-relaxed font-sans"
            placeholder="Type your prompt here..."
            value={value}
            onChange={e => onChange(e.target.value)}
            onKeyDown={e => {
              if (e.key === 'Enter' && !e.shiftKey && !e.metaKey && !e.ctrlKey) {
                e.preventDefault()
                onSend()
                onClose()
              }
            }}
          />
        </div>

        {/* Footer Actions */}
        <div className="px-6 py-4 bg-surface-2 border-t border-border-subtle flex items-center justify-between">
          <div className="flex items-center gap-4">
            <label className="flex items-center gap-2 cursor-pointer select-none">
              <input
                type="checkbox"
                className="w-4 h-4 rounded border-border-default bg-surface-3 text-brand-500 focus:ring-brand-500 focus:ring-offset-surface-2"
                checked={excludeContext}
                onChange={e => onExcludeContextChange(e.target.checked)}
              />
              <span className="text-sm text-text-secondary">Exclude current file context</span>
            </label>
          </div>

          <div className="flex items-center gap-2">
            <button
              onClick={onPreview}
              className="flex items-center gap-2 px-4 py-2 rounded-lg bg-surface-3 hover:bg-surface-4 text-text-secondary hover:text-text-primary border border-border-default transition-all font-medium text-sm"
            >
              <Eye size={16} />
              <span>Preview</span>
            </button>
            <button
              onClick={() => {
                onSend()
                onClose()
              }}
              disabled={!value.trim()}
              className="flex items-center gap-2 px-5 py-2 rounded-lg bg-brand-500 hover:bg-brand-600 disabled:opacity-50 disabled:cursor-not-allowed text-brand-foreground shadow-lg shadow-brand-500/20 transition-all font-medium text-sm"
            >
              <Send size={16} />
              <span>Send Prompt</span>
            </button>
          </div>
        </div>
      </div>
    </div>,
    document.body,
  )
}
