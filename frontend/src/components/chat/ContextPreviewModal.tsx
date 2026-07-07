import {X, ChevronDown, ChevronRight} from 'lucide-react'
import {useState} from 'react'

interface ContextPreview {
  systemPrompt: string
  contextText: string
  userMessage: string
  estimatedTokens: number
  contextLimit: number
}

interface Props {
  preview: ContextPreview
  onClose: () => void
  onConfirm: () => void
}

export default function ContextPreviewModal({preview, onClose, onConfirm}: Props) {
  const [expandSystem, setExpandSystem] = useState(false)
  const [expandContext, setExpandContext] = useState(true)

  const usage = ((preview.estimatedTokens / preview.contextLimit) * 100).toFixed(1)

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50">
      <div className="bg-surface-1 border border-border-default rounded-xl w-[560px] max-w-[calc(100vw-2rem)] max-h-[80vh] flex flex-col shadow-xl">
        <div className="flex items-center justify-between px-4 py-3 border-b border-border-default">
          <h3 className="text-sm font-semibold text-text-primary">Context Preview</h3>
          <button onClick={onClose} className="text-text-tertiary hover:text-text-secondary" aria-label="Close">
            <X size={16} />
          </button>
        </div>

        <div className="flex-1 overflow-y-auto p-4 space-y-3">
          <div className="flex items-center justify-between text-xs text-text-secondary tabular-nums">
            <span>Estimated: ~{preview.estimatedTokens.toLocaleString()} tokens</span>
            <span>Context limit: {preview.contextLimit.toLocaleString()} ({usage}% used)</span>
          </div>

          <CollapsibleSection
            title="System Prompt"
            expanded={expandSystem}
            onToggle={() => setExpandSystem(v => !v)}
          >
            <pre className="text-xs text-text-secondary whitespace-pre-wrap font-mono bg-surface-2 rounded-lg p-3 max-h-[200px] overflow-y-auto">
              {preview.systemPrompt}
            </pre>
          </CollapsibleSection>

          <CollapsibleSection
            title="Flow Context"
            expanded={expandContext}
            onToggle={() => setExpandContext(v => !v)}
          >
            <pre className="text-xs text-text-secondary whitespace-pre-wrap font-mono bg-surface-2 rounded-lg p-3 max-h-[300px] overflow-y-auto">
              {preview.contextText || '(no context)'}
            </pre>
          </CollapsibleSection>

          <div>
            <span className="text-xs font-medium text-text-secondary">Your Message</span>
            <div className="mt-1 text-sm text-text-primary bg-surface-2 rounded-lg p-3">
              {preview.userMessage}
            </div>
          </div>
        </div>

        <div className="flex justify-end gap-2 px-4 py-3 border-t border-border-default">
          <button
            className="px-3 py-1.5 text-sm rounded-lg bg-surface-2 hover:bg-surface-3 text-text-secondary transition-colors"
            onClick={onClose}
          >
            Cancel
          </button>
          <button
            className="px-3 py-1.5 text-sm rounded-lg bg-brand-500 hover:bg-brand-600 text-brand-foreground transition-colors"
            onClick={onConfirm}
          >
            Send
          </button>
        </div>
      </div>
    </div>
  )
}

function CollapsibleSection({title, expanded, onToggle, children}: {
  title: string; expanded: boolean; onToggle: () => void; children: React.ReactNode
}) {
  return (
    <div>
      <button
        className="flex items-center gap-1 text-xs font-medium text-text-secondary hover:text-text-primary"
        onClick={onToggle}
      >
        {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        {title}
      </button>
      {expanded && <div className="mt-1">{children}</div>}
    </div>
  )
}
