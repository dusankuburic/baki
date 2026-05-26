import clsx from 'clsx'
import {Plus, X, MessageSquare} from 'lucide-react'
import {useState, useRef, useEffect} from 'react'
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

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollLeft = scrollRef.current.scrollWidth
    }
  }, [threads.length])

  if (threads.length === 0) {
    return null
  }

  return (
    <div className="flex items-center gap-0 border-b border-border-subtle">
      <div ref={scrollRef} className="flex-1 flex items-center overflow-x-auto no-scrollbar">
        {threads.map(thread => (
          <ThreadTab
            key={thread.id}
            thread={thread}
            isActive={thread.id === activeThreadId}
            onSelect={onSelect}
            onClose={onClose}
            onRename={onRename}
          />
        ))}
      </div>
      <button
        className="flex items-center justify-center w-6 h-6 shrink-0 mr-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-text-secondary transition-colors"
        onClick={onCreate}
        title="New chat thread"
      >
        <Plus size={13} />
      </button>
    </div>
  )
}

function ThreadTab({thread, isActive, onSelect, onClose, onRename}: {
  thread: ChatThread
  isActive: boolean
  onSelect: (id: string) => void
  onClose: (id: string) => void
  onRename: (id: string, title: string) => void
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
    if (trimmed && trimmed !== thread.title) {
      onRename(thread.id, trimmed)
    }
    setEditing(false)
  }

  return (
    <div
      className={clsx(
        'group flex items-center gap-1 px-2 py-1.5 cursor-pointer border-b-2 transition-colors shrink-0 max-w-[120px] min-w-[60px]',
        isActive
          ? 'border-brand-500 bg-brand-500/5'
          : 'border-transparent hover:bg-surface-2'
      )}
      onClick={() => !editing && onSelect(thread.id)}
      onDoubleClick={handleDoubleClick}
    >
      <MessageSquare size={11} className={clsx('shrink-0', isActive ? 'text-brand-400' : 'text-text-tertiary')} />
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
        <span className={clsx('truncate text-2xs', isActive ? 'text-text-secondary font-medium' : 'text-text-tertiary')}>
          {label}
        </span>
      )}
      <button
        className={clsx(
          'shrink-0 p-0.5 rounded transition-all',
          isActive ? 'opacity-60 hover:opacity-100 hover:bg-surface-3' : 'opacity-0 group-hover:opacity-60 hover:!opacity-100 hover:bg-surface-3'
        )}
        onClick={e => { e.stopPropagation(); onClose(thread.id) }}
        title="Close thread"
      >
        <X size={10} />
      </button>
    </div>
  )
}
