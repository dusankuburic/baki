import {FileText, MessageSquare} from 'lucide-react'

interface Props {
  hasDoc: boolean
  hasThread: boolean
  onCreateThread?: () => void
}

export default function EmptyChatState({hasDoc, hasThread, onCreateThread}: Props) {
  if (hasDoc && hasThread) return null

  return (
    <div className="flex-1 flex flex-col items-center justify-center px-8 text-center animate-fade-in">
      <div className="flex flex-col items-center gap-4 max-w-sm">
        {!hasDoc ? (
          <>
            <div className="w-12 h-12 rounded-full bg-surface-2 flex items-center justify-center">
              <FileText size={24} className="text-text-tertiary" />
            </div>
            <div>
              <h3 className="text-sm font-medium text-text-secondary mb-1">
                No flow loaded
              </h3>
              <p className="text-xs text-text-tertiary">
                Open or drag-drop a .pad file to begin
              </p>
            </div>
          </>
        ) : (
          <>
            <div className="w-12 h-12 rounded-full bg-surface-2 flex items-center justify-center">
              <MessageSquare size={24} className="text-text-tertiary" />
            </div>
            <div>
              <h3 className="text-sm font-medium text-text-secondary mb-1">
                No active conversation
              </h3>
              <p className="text-xs text-text-tertiary mb-3">
                Start a conversation to analyze your flow with AI
              </p>
              {onCreateThread && (
                <button
                  onClick={onCreateThread}
                  className="px-4 py-2 bg-brand-600 hover:bg-brand-700 text-brand-foreground text-xs font-medium rounded-lg transition-colors"
                >
                  Start a conversation
                </button>
              )}
            </div>
          </>
        )}
      </div>
    </div>
  )
}
