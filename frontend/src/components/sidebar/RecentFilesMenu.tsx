import {useTranslation} from 'react-i18next'
import {useRef, useEffect, useState, useCallback, useMemo} from 'react'
import {FileText, Folder, X, Trash2} from 'lucide-react'
import clsx from 'clsx'
import type {RecentFile} from '@/types'
import {relativeTime, absoluteTime} from '@/lib/time'
import {Portal} from '@/components/shared'

type RecentFilesMenuProps = {
  files: RecentFile[]
  anchorRef: React.RefObject<HTMLElement | null>
  onSelect: (path: string) => void
  onRemove: (path: string) => void
  onClear: () => void
  onClose: () => void
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

function RecentRow({
  file,
  isActive,
  onSelect,
  onRemove,
}: {
  file: RecentFile
  isActive: boolean
  onSelect: (path: string) => void
  onRemove: (path: string) => void
}) {
  const {t} = useTranslation('shell')
  return (
    <div
      role="option"
      aria-selected={isActive}
      className={clsx(
        'group flex items-center gap-2 px-3 py-2 cursor-pointer transition-colors duration-fast',
        isActive ? 'bg-surface-3' : 'hover:bg-surface-3',
      )}
      onClick={() => onSelect(file.path)}
    >
      {file.isFolder ? (
        <Folder size={14} className="text-brand-400 flex-shrink-0" />
      ) : (
        <FileText size={14} className="text-text-tertiary flex-shrink-0" />
      )}
      <div className="flex-1 min-w-0">
        <div className="text-sm text-text-primary truncate">{file.name}</div>
        <div className="text-2xs text-text-tertiary truncate">{file.path}</div>
      </div>
      <div className="flex flex-col items-end gap-0.5 shrink min-w-0 max-w-[56px]">
        <span title={absoluteTime(file.lastOpen)} className="text-2xs text-text-tertiary truncate">
          {relativeTime(file.lastOpen)}
        </span>
        {!file.isFolder && <span className="text-2xs text-text-tertiary truncate">{formatSize(file.size)}</span>}
      </div>
      <button
        onClick={e => {
          e.stopPropagation()
          onRemove(file.path)
        }}
        className="opacity-0 group-hover:opacity-100 w-5 h-5 flex items-center justify-center rounded-sm text-text-tertiary hover:text-semantic-error hover:bg-surface-4 transition-all duration-fast"
        aria-label={t('recent.remove')}
      >
        <X size={10} />
      </button>
    </div>
  )
}

export default function RecentFilesMenu({
  files,
  anchorRef,
  onSelect,
  onRemove,
  onClear,
  onClose,
}: RecentFilesMenuProps) {
  const {t} = useTranslation('shell')
  const menuRef = useRef<HTMLDivElement>(null)
  const [pos, setPos] = useState({top: 0, left: 0, width: 0})
  const [activeIndex, setActiveIndex] = useState(-1)

  const folders = useMemo(() => files.filter(f => f.isFolder), [files])
  const items = useMemo(() => files.filter(f => !f.isFolder), [files])
  const isEmpty = files.length === 0
  // Keyboard nav walks folders-then-files in the same order they're
  // rendered, so ArrowDown/ArrowUp step through the visual grouping.
  const flatOrder = useMemo(() => [...folders, ...items], [folders, items])

  const updatePosition = useCallback(() => {
    if (!anchorRef.current) return
    const rect = anchorRef.current.getBoundingClientRect()
    setPos({top: rect.bottom + 4, left: rect.left, width: rect.width})
  }, [anchorRef])

  useEffect(() => {
    updatePosition()
    window.addEventListener('resize', updatePosition)
    window.addEventListener('scroll', updatePosition, true)
    return () => {
      window.removeEventListener('resize', updatePosition)
      window.removeEventListener('scroll', updatePosition, true)
    }
  }, [updatePosition])

  // Focus the menu on mount so it accepts arrow-key navigation immediately.
  // (React's autoFocus prop only takes effect on form controls, not divs.)
  useEffect(() => {
    menuRef.current?.focus()
  }, [])

  useEffect(() => {
    const handler = (e: MouseEvent) => {
      const target = e.target as Node
      if (!menuRef.current || menuRef.current.contains(target)) return
      // Clicks on the anchor (the header row with the chevron) are the
      // toggle's job — closing here too would make the chevron's click
      // reopen the menu it just closed.
      if (anchorRef.current?.contains(target)) return
      onClose()
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [onClose, anchorRef])

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (flatOrder.length === 0) {
        if (e.key === 'Escape') onClose()
        return
      }
      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault()
          setActiveIndex(i => Math.min((i < 0 ? -1 : i) + 1, flatOrder.length - 1))
          break
        case 'ArrowUp':
          e.preventDefault()
          setActiveIndex(i => Math.max(i - 1, 0))
          break
        case 'Enter': {
          e.preventDefault()
          const f = flatOrder[activeIndex]
          if (f) onSelect(f.path)
          break
        }
        case 'Escape':
          e.preventDefault()
          onClose()
          break
      }
    },
    [flatOrder, activeIndex, onSelect, onClose],
  )

  return (
    <Portal>
      <div
        ref={menuRef}
        className="fixed bg-surface-2 border border-border-default rounded-lg shadow-lg z-overlay animate-fade-in overflow-hidden"
        style={{top: pos.top, left: pos.left, width: pos.width}}
        role="listbox"
        aria-label={t('recent.menuAria')}
        tabIndex={-1}
        onKeyDown={handleKeyDown}
      >
        <div className="flex items-center justify-between px-3 py-2 border-b border-border-subtle">
          <span className="text-xs font-medium text-text-secondary">{t('recent.heading')}</span>
          {!isEmpty && (
            <button
              onClick={() => {
                onClear()
                onClose()
              }}
              className="text-2xs text-text-tertiary hover:text-semantic-error flex items-center gap-1"
            >
              <Trash2 size={10} />
              Clear all
            </button>
          )}
        </div>

        {isEmpty ? (
          <div className="px-3 py-4 text-xs text-text-tertiary text-center">{t('recent.empty')}</div>
        ) : (
          <div className="max-h-72 overflow-y-auto">
            {folders.length > 0 && (
              <>
                <div className="px-3 pt-2 pb-1">
                  <span className="text-2xs font-semibold uppercase tracking-wider text-text-disabled">
                    {t('recent.folders')}
                  </span>
                </div>
                {folders.map(file => (
                  <RecentRow
                    key={file.path}
                    file={file}
                    isActive={flatOrder[activeIndex]?.path === file.path}
                    onSelect={onSelect}
                    onRemove={onRemove}
                  />
                ))}
              </>
            )}

            {items.length > 0 && (
              <>
                <div className={`px-3 pb-1 ${folders.length > 0 ? 'pt-2 border-t border-border-subtle mt-1' : 'pt-2'}`}>
                  <span className="text-2xs font-semibold uppercase tracking-wider text-text-disabled">
                    {t('recent.files')}
                  </span>
                </div>
                {items.map(file => (
                  <RecentRow
                    key={file.path}
                    file={file}
                    isActive={flatOrder[activeIndex]?.path === file.path}
                    onSelect={onSelect}
                    onRemove={onRemove}
                  />
                ))}
              </>
            )}
          </div>
        )}
      </div>
    </Portal>
  )
}
