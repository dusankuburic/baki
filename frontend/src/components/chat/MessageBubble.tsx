import clsx from 'clsx'
import {Copy, Check, RefreshCw, RotateCcw, Bot, User} from 'lucide-react'
import {useState, useCallback, memo} from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type {ChatMessage as ChatMessageType} from '@/types/domain'
import CodeBlock from './CodeBlock'

interface Props {
  message: ChatMessageType
  isStreaming?: boolean
  isThinking?: boolean
  isLastAssistant?: boolean
  onRegenerate?: () => void
  onRetry?: () => void
}

function formatTime(ts: string): string {
  try {
    const d = new Date(ts)
    return d.toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'})
  } catch {
    return ''
  }
}

const MentionPill = ({ path }: { path: string }) => {
  const parts = path.split(/[/\\]/)
  const filename = parts[parts.length - 1]
  return (
    <span className="inline-flex items-center px-1.5 py-0.5 rounded-md bg-brand-500/10 text-brand-400 border border-brand-500/20 font-medium text-[0.9em] mx-0.5 select-none">
      @{filename}
    </span>
  )
}

function renderContent(content: string, isUser: boolean, isStreaming?: boolean) {
  // Regex to match @mentions (support paths with backslashes)
  const mentionRegex = /@([a-zA-Z0-9_./\\\-]+)/g

  if (isUser) {
    const parts = content.split(mentionRegex)
    const matches = content.match(mentionRegex) || []
    
    return (
      <div className="whitespace-pre-wrap break-words">
        {parts.map((part, i) => (
          <span key={i}>
            {part}
            {matches[i] && <MentionPill path={matches[i].slice(1)} />}
          </span>
        ))}
      </div>
    )
  }

  // For AI messages, we use ReactMarkdown but we need to handle the mentions.
  // One way is to pre-process the content to wrap mentions in a custom syntax or just replace them after rendering if possible.
  // A cleaner way for ReactMarkdown is to split the text nodes.
  
  return (
    <div className="prose-chat break-words">
      <ReactMarkdown 
        remarkPlugins={[remarkGfm]}
        components={{
          code({node, inline, className, children, ...props}: any) {
            const match = /language-(\w+)/.exec(className || '')
            return !inline ? (
              <CodeBlock
                language={match ? match[1] : ''}
                value={String(children).replace(/\n$/, '')}
                {...props}
              />
            ) : (
              <code className="px-1.5 py-0.5 rounded bg-surface-3 text-brand-300 font-mono text-[0.85em]" {...props}>
                {children}
              </code>
            )
          },
          // Custom renderer for text to handle mentions
          text({ children }: any) {
            const text = String(children)
            if (!text.includes('@')) return <>{text}</>
            
            const parts = text.split(mentionRegex)
            const matches = text.match(mentionRegex) || []
            
            return (
              <>
                {parts.map((part, i) => (
                  <span key={i}>
                    {part}
                    {matches[i] && <MentionPill path={matches[i].slice(1)} />}
                  </span>
                ))}
              </>
            )
          }
        }}
      >
        {content}
      </ReactMarkdown>
      {isStreaming && (
        <span className="streaming-cursor inline-block w-[3px] h-[1.2em] bg-brand-400 ml-0.5 align-text-bottom" />
      )}
    </div>
  )
}

function MessageBubble({message, isStreaming, isThinking, isLastAssistant, onRegenerate, onRetry}: Props) {
  const isUser = message.role === 'user'
  const isError = message.finishReason === 'error'
  const [copied, setCopied] = useState(false)
  const [showActions, setShowActions] = useState(false)

  const handleCopy = useCallback(() => {
    navigator.clipboard.writeText(message.content).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    }).catch(() => {})
  }, [message.content])

  const time = formatTime(message.timestamp)

  if (isThinking) {
    return (
      <div className="flex flex-col items-start gap-1 animate-fade-in">
        <div className="flex items-center gap-1.5 px-1">
          <Bot size={11} className="text-text-tertiary" />
          <span className="text-2xs font-medium text-text-tertiary">AI</span>
        </div>
        <div className="px-4 py-3 bg-surface-2 border border-border-subtle rounded-2xl rounded-tl-md">
          <div className="flex items-center gap-2">
            <div className="flex gap-1">
              <span className="typing-dot w-1.5 h-1.5 rounded-full bg-text-tertiary" style={{animationDelay: '0ms'}} />
              <span className="typing-dot w-1.5 h-1.5 rounded-full bg-text-tertiary" style={{animationDelay: '150ms'}} />
              <span className="typing-dot w-1.5 h-1.5 rounded-full bg-text-tertiary" style={{animationDelay: '300ms'}} />
            </div>
            <span className="text-xs text-text-tertiary">Thinking...</span>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div
      className={clsx('group flex flex-col gap-1 animate-message-in', isUser ? 'items-end' : 'items-start')}
      onMouseEnter={() => setShowActions(true)}
      onMouseLeave={() => setShowActions(false)}
    >
      <div className="flex items-center gap-1.5 px-1">
        {isUser ? (
          <User size={11} className="text-text-tertiary" />
        ) : (
          <Bot size={11} className="text-text-tertiary" />
        )}
        <span className="text-2xs font-medium text-text-tertiary">
          {isUser ? 'You' : 'AI'}
        </span>
        {message.model && !isUser && (
          <span className="text-2xs text-text-tertiary/60">· {message.model}</span>
        )}
        {time && (
          <span className="text-2xs text-text-tertiary/40">{time}</span>
        )}
      </div>

      <div
        className={clsx(
          'relative px-3 py-2.5 max-w-full text-sm leading-relaxed',
          isUser
            ? 'bg-brand-500/12 border border-brand-500/20 rounded-2xl rounded-tr-sm'
            : isError
            ? 'bg-red-500/8 border border-red-500/15 rounded-2xl rounded-tl-sm'
            : 'bg-surface-2 border border-border-subtle rounded-2xl rounded-tl-sm',
          isStreaming && 'border-brand-500/20'
        )}
      >
        {renderContent(message.content, isUser, isStreaming)}
      </div>

      {!isStreaming && (
        <div className={clsx(
          'flex items-center gap-1 px-1 transition-opacity duration-150',
          showActions ? 'opacity-100' : 'opacity-0'
        )}>
          <button
            className="p-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-text-secondary transition-colors"
            onClick={handleCopy}
            aria-label={copied ? 'Copied' : 'Copy'}
          >
            {copied ? <Check size={11} className="text-green-400" /> : <Copy size={11} />}
          </button>
          {isError && onRetry && (
            <button
              className="flex items-center gap-1 p-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-red-400 transition-colors text-2xs"
              onClick={onRetry}
            >
              <RotateCcw size={11} />
              <span>Retry</span>
            </button>
          )}
          {isLastAssistant && !isError && onRegenerate && (
            <button
              className="flex items-center gap-1 p-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-text-secondary transition-colors text-2xs"
              onClick={onRegenerate}
            >
              <RefreshCw size={11} />
              <span>Regenerate</span>
            </button>
          )}
        </div>
      )}
    </div>
  )
}

// Memoize to prevent re-renders during streaming - only re-render when props change
export default memo(MessageBubble, (prevProps, nextProps) => {
  return (
    prevProps.message.id === nextProps.message.id &&
    prevProps.message.content === nextProps.message.content &&
    prevProps.isStreaming === nextProps.isStreaming &&
    prevProps.isThinking === nextProps.isThinking &&
    prevProps.isLastAssistant === nextProps.isLastAssistant &&
    prevProps.onRegenerate === nextProps.onRegenerate &&
    prevProps.onRetry === nextProps.onRetry
  )
})
