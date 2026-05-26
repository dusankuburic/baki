import {useRef, useEffect} from 'react'
import {FileText, Folder, X, Trash2} from 'lucide-react'
import type {RecentFile} from '@/types/domain'

type RecentFilesMenuProps = {
    files: RecentFile[]
    onSelect: (path: string) => void
    onRemove: (path: string) => void
    onClear: () => void
    onClose: () => void
}

function formatRelativeTime(dateStr: string): string {
    const date = new Date(dateStr)
    const now = Date.now()
    const diff = now - date.getTime()
    const minutes = Math.floor(diff / 60000)
    if (minutes < 1) return 'just now'
    if (minutes < 60) return `${minutes}m ago`
    const hours = Math.floor(minutes / 60)
    if (hours < 24) return `${hours}h ago`
    const days = Math.floor(hours / 24)
    if (days < 7) return `${days}d ago`
    return date.toLocaleDateString()
}

function formatSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

function RecentRow({file, onSelect, onRemove}: {
    file: RecentFile
    onSelect: (path: string) => void
    onRemove: (path: string) => void
}) {
    return (
        <div
            className="group flex items-center gap-2 px-3 py-2 hover:bg-surface-3 cursor-pointer transition-colors duration-fast"
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
                <span className="text-2xs text-text-tertiary truncate">{formatRelativeTime(file.lastOpen)}</span>
                {!file.isFolder && (
                    <span className="text-2xs text-text-tertiary truncate">{formatSize(file.size)}</span>
                )}
            </div>
            <button
                onClick={e => { e.stopPropagation(); onRemove(file.path) }}
                className="opacity-0 group-hover:opacity-100 w-5 h-5 flex items-center justify-center rounded-sm text-text-tertiary hover:text-semantic-error hover:bg-surface-4 transition-all duration-fast"
                aria-label="Remove"
            >
                <X size={10} />
            </button>
        </div>
    )
}

export default function RecentFilesMenu({files, onSelect, onRemove, onClear, onClose}: RecentFilesMenuProps) {
    const menuRef = useRef<HTMLDivElement>(null)

    useEffect(() => {
        const handler = (e: MouseEvent) => {
            if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
                onClose()
            }
        }
        document.addEventListener('mousedown', handler)
        return () => document.removeEventListener('mousedown', handler)
    }, [onClose])

    const folders = files.filter(f => f.isFolder)
    const items   = files.filter(f => !f.isFolder)
    const isEmpty = files.length === 0

    return (
        <div
            ref={menuRef}
            className="absolute left-0 right-0 top-full mt-1 bg-surface-2 border border-border-default rounded-lg shadow-lg z-overlay animate-fade-in overflow-hidden"
        >
            <div className="flex items-center justify-between px-3 py-2 border-b border-border-subtle">
                <span className="text-xs font-medium text-text-secondary">Recent</span>
                {!isEmpty && (
                    <button
                        onClick={() => { onClear(); onClose() }}
                        className="text-2xs text-text-tertiary hover:text-semantic-error flex items-center gap-1"
                    >
                        <Trash2 size={10} />
                        Clear all
                    </button>
                )}
            </div>

            {isEmpty ? (
                <div className="px-3 py-4 text-xs text-text-tertiary text-center">No recent items</div>
            ) : (
                <div className="max-h-72 overflow-y-auto">
                    {folders.length > 0 && (
                        <>
                            <div className="px-3 pt-2 pb-1">
                                <span className="text-[10px] font-semibold uppercase tracking-wider text-text-disabled">
                                    Folders
                                </span>
                            </div>
                            {folders.map(file => (
                                <RecentRow key={file.path} file={file} onSelect={onSelect} onRemove={onRemove} />
                            ))}
                        </>
                    )}

                    {items.length > 0 && (
                        <>
                            <div className={`px-3 pb-1 ${folders.length > 0 ? 'pt-2 border-t border-border-subtle mt-1' : 'pt-2'}`}>
                                <span className="text-[10px] font-semibold uppercase tracking-wider text-text-disabled">
                                    Files
                                </span>
                            </div>
                            {items.map(file => (
                                <RecentRow key={file.path} file={file} onSelect={onSelect} onRemove={onRemove} />
                            ))}
                        </>
                    )}
                </div>
            )}
        </div>
    )
}
