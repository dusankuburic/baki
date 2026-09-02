import clsx from 'clsx'
import {Plus, X, MessageSquare, Wrench, CircleCheck} from 'lucide-react'
import {useState, useRef, useEffect, useMemo} from 'react'
import {useChatStore} from '@/stores/chatStore'
import type {ChatThread} from '@/stores/chatStore'

interface Props {
  threads: ChatThread[]
  activeThreadId: string | null
  onSelect: (threadId: string) => void
  onCreate: () => void
  onClose: (threadId: string) => void
  onRename: (threadId: string, title: string) => void
}

export default function ChatThreadBar({threads, activeThreadId, onSelect, onCreate, onClose, onRename}: Props) {
  const scrollRef = useRef<HTMLDivElement>(null)
  // Streaming thread ids — a Set so the `.has()` lookup in the render loop is
  // O(1) and the selector only emits a new reference when the set of streaming
  // threads actually changes (keys join into a stable string key).
  const streamingKey = useChatStore(s => Object.keys(s.streams).sort().join('|'))
  const streamingIds = useMemo(() => new Set(streamingKey ? streamingKey.split('|') : []), [streamingKey])

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollLeft = scrollRef.current.scrollWidth
    }
  }, [threads.length])

  if (threads.length === 0) {
    return null
  }

  // Left/Right arrow keys move between tabs (roving focus), matching the
  // WAI-ARIA tabs pattern. Only the active tab is in the tab order (tabIndex 0);
  // the rest are -1 and reached via arrows.
  const handleTabKeyDown = (e: React.KeyboardEvent, index: number) => {
    if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return
    e.preventDefault()
    const delta = e.key === 'ArrowRight' ? 1 : -1
    const nextIndex = (index + delta + threads.length) % threads.length
    onSelect(threads[nextIndex].id)
  }

  return (
    <div className="flex items-center gap-0 border-b border-border-subtle">
      <div
        ref={scrollRef}
        className="flex-1 flex items-center overflow-x-auto no-scrollbar"
        role="tablist"
        aria-label="Chat threads"
      >
        {threads.map((thread, i) => (
          <ThreadTab
            key={thread.id}
            thread={thread}
            isActive={thread.id === activeThreadId}
            isStreaming={streamingIds.has(thread.id)}
            onSelect={onSelect}
            onClose={onClose}
            onRename={onRename}
            onKeyDown={e => handleTabKeyDown(e, i)}
          />
        ))}
      </div>
      <button
        className="flex items-center justify-center w-6 h-6 shrink-0 mr-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-text-secondary transition-colors"
        onClick={onCreate}
        title="New chat thread"
        aria-label="New chat thread"
      >
        <Plus size={13} />
      </button>
    </div>
  )
}

function ThreadTab({
  thread,
  isActive,
  isStreaming,
  onSelect,
  onClose,
  onRename,
  onKeyDown,
}: {
  thread: ChatThread
  isActive: boolean
  isStreaming: boolean
  onSelect: (id: string) => void
  onClose: (id: string) => void
  onRename: (id: string, title: string) => void
  onKeyDown: (e: React.KeyboardEvent) => void
}) {
  const [editing, setEditing] = useState(false)
  const [editValue, setEditValue] = useState(thread.title)
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (editing && inputRef.current) {
      inputRef.current.focus()
      inputRef.current.select()
    }
  }, [editing])

  const label = thread.title || 'New chat'

  const handleDoubleClick = () => {
    setEditing(true)
    setEditValue(thread.title)
  }

  const commitEdit = () => {
    const trimmed = editValue.trim()
    if (!trimmed) {
      setEditValue(thread.title)
      setEditing(false)
      return
    }
    if (trimmed !== thread.title) {
      onRename(thread.id, trimmed)
    }
    setEditing(false)
  }

  return (
    <div
      role="tab"
      aria-selected={isActive}
      tabIndex={isActive ? 0 : -1}
      className={clsx(
        'group flex items-center gap-1 px-2 py-1.5 cursor-pointer border-b-2 transition-colors shrink-0 max-w-[120px] min-w-[60px] outline-none focus-visible:ring-1 focus-visible:ring-brand-500',
        isActive ? 'border-brand-500 bg-brand-500/5' : 'border-transparent hover:bg-surface-2',
      )}
      onClick={() => !editing && onSelect(thread.id)}
      onDoubleClick={handleDoubleClick}
      onKeyDown={e => {
        if (!editing) onKeyDown(e)
      }}
    >
      <MessageSquare size={11} className={clsx('shrink-0', isActive ? 'text-brand-400' : 'text-text-tertiary')} />
      {isStreaming && (
        <span className="shrink-0 w-1.5 h-1.5 rounded-full bg-brand-400 animate-pulse" title="Generating…" />
      )}
      {/* Agentic badges: which conversations ran tools / landed approved
          fixes. aria-labels carry the meaning; the glyphs are decorative. */}
      {thread.appliedFixes ? (
        <CircleCheck size={10} className="shrink-0 text-semantic-success/80" aria-label="Fixes applied in this thread" role="img" />
      ) : thread.usedTools ? (
        <Wrench size={10} className="shrink-0 text-text-tertiary/70" aria-label="Tools used in this thread" role="img" />
      ) : null}
      {editing ? (
        <input
          ref={inputRef}
          className="flex-1 min-w-0 bg-surface-2 border border-border-default rounded px-1 py-0 text-2xs text-text-primary outline-none focus:border-brand-500"
          value={editValue}
          onChange={e => setEditValue(e.target.value)}
          onBlur={commitEdit}
          onKeyDown={e => {
            if (e.key === 'Enter') commitEdit()
            if (e.key === 'Escape') setEditing(false)
          }}
          onClick={e => e.stopPropagation()}
        />
      ) : (
        <span
          title={thread.title || 'New chat'}
          className={clsx('truncate text-2xs', isActive ? 'text-text-secondary font-medium' : 'text-text-tertiary')}
        >
          {label}
        </span>
      )}
      <button
        className={clsx(
          'shrink-0 p-0.5 rounded transition-all duration-fast',
          isActive
            ? 'opacity-60 hover:opacity-100 hover:bg-surface-3'
            : 'opacity-0 group-hover:opacity-60 hover:!opacity-100 hover:bg-surface-3',
        )}
        onClick={e => {
          e.stopPropagation()
          onClose(thread.id)
        }}
        title="Close thread"
        aria-label={`Close ${label}`}
      >
        <X size={10} />
      </button>
    </div>
  )
}
