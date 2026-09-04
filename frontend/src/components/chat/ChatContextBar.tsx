import {useTranslation} from 'react-i18next'
import {useMemo} from 'react'
import clsx from 'clsx'
import {Check, ChevronDown, Crosshair, Files, FileText, X} from 'lucide-react'
import {Dropdown, type DropdownItem} from '@/components/shared'
import type {SourceFileInfo} from '@/types'

interface Props {
  // Block scope, when the thread is focused on one block.
  contextBlockId: string | null
  blockName?: string
  blockType?: string
  onClearBlock: () => void
  // The block currently selected on the canvas, offered as a scope when the
  // thread is still flow-wide.
  selectedBlockId: string | null
  onFocusBlock: (blockId: string) => void
  // Per-thread source-file selection.
  files: SourceFileInfo[]
  selectedFiles: string[]
  onFilesChange: (filenames: string[]) => void
}

// ChatContextBar collapses three stacked full-width rows — the block-scope
// chip, the "Scope: entire flow" line, and the source-file picker — into one
// wrapping row of chips that renders only when it has something to say.
export default function ChatContextBar({
  contextBlockId,
  blockName,
  blockType,
  onClearBlock,
  selectedBlockId,
  onFocusBlock,
  files,
  selectedFiles,
  onFilesChange,
}: Props) {
  const {t} = useTranslation('chat')

  const selected = useMemo(() => new Set(selectedFiles), [selectedFiles])
  const allSelected = files.length > 0 && selectedFiles.length === files.length

  const fileItems = useMemo<DropdownItem[]>(() => {
    if (files.length === 0) return []
    return [
      {
        type: 'item',
        label: allSelected ? t('files.selectNone') : t('files.selectAll'),
        icon: Files,
        onSelect: () => onFilesChange(allSelected ? [] : files.map(f => f.filename)),
      },
      {type: 'separator'},
      ...files.map(f => ({
        type: 'item' as const,
        label: `${f.subflowName} · ${t('files.blockCount', {count: f.blockCount})}`,
        icon: selected.has(f.filename) ? Check : undefined,
        onSelect: () =>
          onFilesChange(
            selected.has(f.filename) ? selectedFiles.filter(s => s !== f.filename) : [...selectedFiles, f.filename],
          ),
      })),
    ]
  }, [files, selected, selectedFiles, allSelected, onFilesChange, t])

  const showScope = !!contextBlockId || !!selectedBlockId
  const showFiles = files.length > 0
  if (!showScope && !showFiles) return null

  return (
    <div className="flex flex-wrap items-center gap-1 px-2 py-1 border-b border-border-subtle">
      {contextBlockId ? (
        <span className="flex items-center gap-1 max-w-full px-1.5 py-0.5 rounded-md bg-brand-500/10 border border-brand-500/20 text-2xs text-text-secondary">
          <Crosshair size={10} className="shrink-0 text-brand-400" />
          <span className="truncate" title={blockType ? `${blockName} (${blockType})` : blockName}>
            {blockName || contextBlockId}
          </span>
          <button
            className="shrink-0 p-0.5 -mr-0.5 rounded hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors"
            onClick={onClearBlock}
            aria-label={t('a11y.clearContext')}
          >
            <X size={10} />
          </button>
        </span>
      ) : selectedBlockId ? (
        <button
          className="flex items-center gap-1 px-1.5 py-0.5 rounded-md border border-border-subtle text-2xs text-text-tertiary hover:text-brand-400 hover:border-brand-500/30 transition-colors"
          onClick={() => onFocusBlock(selectedBlockId)}
          title={t('context.focusSelectedTitle')}
        >
          <Crosshair size={10} className="shrink-0" />
          {t('context.focusSelected')}
        </button>
      ) : null}

      {showFiles && (
        <Dropdown
          className="min-w-0"
          items={fileItems}
          trigger={
            <button
              type="button"
              className={clsx(
                'flex items-center gap-1 px-1.5 py-0.5 rounded-md border text-2xs transition-colors',
                selectedFiles.length > 0
                  ? 'border-border-subtle text-text-secondary hover:bg-surface-2'
                  : 'border-border-subtle text-text-tertiary hover:bg-surface-2',
              )}
            >
              <FileText size={10} className="shrink-0" />
              <span className="truncate">
                {selectedFiles.length === 0
                  ? t('files.noneSelected')
                  : allSelected
                    ? t('files.allSelected', {count: files.length})
                    : t('files.someSelected', {selected: selectedFiles.length, total: files.length})}
              </span>
              <ChevronDown size={10} className="shrink-0 text-text-tertiary" />
            </button>
          }
        />
      )}
    </div>
  )
}
