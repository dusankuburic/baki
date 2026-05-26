import {FileText, MessageSquare} from 'lucide-react'

interface Props {
  hasDoc: boolean
  hasThread: boolean
}

export default function EmptyChatState({hasDoc, hasThread}: Props) {
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
                Open a PAD flow file to start analyzing with AI
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
              <p className="text-xs text-text-tertiary">
                Click "New chat" to start a new conversation
              </p>
            </div>
          </>
        )}
      </div>
    </div>
  )
}
