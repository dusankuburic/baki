import {useState, useCallback, useRef, useEffect} from 'react'
import clsx from 'clsx'
import {Search, X, ChevronRight, FileCode} from 'lucide-react'
import {useFlowStore} from '@/stores/flowStore'
import {flowApi} from '@/api'
import {useDebouncedSearch} from '@/hooks/useDebouncedSearch'
import {useListNavigation} from '@/hooks/useListNavigation'
import {toggleSetMember} from '@/lib/collections'
import {logger} from '@/lib/logger'
import type {SearchResult, BlockType} from '@/types'

type GlobalSearchOverlayProps = {
    isOpen: boolean
    onClose: () => void
}

const FILTER_TYPES: {type: BlockType; label: string}[] = [
    {type: 'ACTION',        label: 'Action'},
    {type: 'CONDITION',     label: 'If'},
    {type: 'LOOP',          label: 'Loop'},
    {type: 'SWITCH',        label: 'Switch'},
    {type: 'VARIABLE',      label: 'Var'},
    {type: 'ERROR_HANDLER', label: 'Error'},
    {type: 'COMMENT',       label: 'Comment'},
]

export default function GlobalSearchOverlay({isOpen, onClose}: GlobalSearchOverlayProps) {
    const [query, setQuery] = useState('')
    const [results, setResults] = useState<SearchResult[]>([])
    const [isSearching, setIsSearching] = useState(false)
    const [activeTypes, setActiveTypes] = useState<Set<BlockType>>(new Set())
    const inputRef = useRef<HTMLInputElement>(null)
    const listRef = useRef<HTMLDivElement>(null)
    const document = useFlowStore(s => s.document)
    const selectBlock = useFlowStore(s => s.selectBlock)
    const selectSubflow = useFlowStore(s => s.selectSubflow)

    const handleSelect = useCallback((result: SearchResult) => {
        selectBlock(result.blockId)
        selectSubflow(result.subflowId)
        onClose()
    }, [selectBlock, selectSubflow, onClose])

    const {activeIndex, setActiveIndex, handleKeyDown} = useListNavigation({
        count: results.length,
        onSelect: (i) => { if (results[i]) handleSelect(results[i]) },
        onClose,
    })

    useEffect(() => {
        if (isOpen) {
            setQuery('')
            setResults([])
            setActiveIndex(0)
            setActiveTypes(new Set())
            requestAnimationFrame(() => inputRef.current?.focus())
        }
    }, [isOpen, setActiveIndex])

    const toggleType = useCallback((type: BlockType) => {
        setActiveTypes(prev => toggleSetMember(prev, type))
    }, [])

    const searchReqIdRef = useRef(0)
    const handleSearch = useCallback(async (q: string, types: Set<BlockType>) => {
        if (!document || !q) {
            setResults([])
            return
        }
        const reqId = ++searchReqIdRef.current
        setIsSearching(true)
        try {
            const res = await flowApi.searchFlow(document.id, {
                text: q,
                fuzzy: true,
                maxResults: 50,
                blockTypes: types.size > 0 ? Array.from(types) : undefined,
            })
            if (reqId !== searchReqIdRef.current) return
            setResults(res.results as SearchResult[])
            setActiveIndex(0)
        } catch (err) {
            if (reqId !== searchReqIdRef.current) return
            logger.warn('Global search failed:', err)
        } finally {
            if (reqId === searchReqIdRef.current) setIsSearching(false)
        }
    }, [document, setActiveIndex])

    const {search: debouncedSearch} = useDebouncedSearch({
        onSearch: (q) => handleSearch(q, activeTypes),
    })

    // Re-run search when type filters change
    useEffect(() => {
        if (query) handleSearch(query, activeTypes)
    // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [activeTypes])

    useEffect(() => {
        const active = listRef.current?.querySelector('[data-active="true"]')
        active?.scrollIntoView({block: 'nearest'})
    }, [activeIndex])

    if (!isOpen) return null

    return (
        <div className="fixed inset-0 z-modal flex items-start justify-center pt-[15vh]" onClick={onClose}>
            <div className="absolute inset-0 bg-surface-overlay backdrop-blur-sm" />
            <div
                className="relative w-full max-w-[720px] bg-surface-1 border border-border-default rounded-xl shadow-2xl overflow-hidden animate-palette flex flex-col max-h-[70vh]"
                onClick={e => e.stopPropagation()}
            >
                {/* Search input row */}
                <div className="flex items-center px-4 border-b border-border-subtle bg-surface-2">
                    <Search className="text-text-tertiary mr-3 flex-shrink-0" size={18} />
                    <input
                        ref={inputRef}
                        type="text"
                        value={query}
                        onChange={e => {
                            setQuery(e.target.value)
                            debouncedSearch(e.target.value)
                        }}
                        onKeyDown={handleKeyDown}
                        placeholder="Search across all subflows and properties..."
                        className="flex-1 bg-transparent py-4 text-base outline-none text-text-primary placeholder:text-text-disabled"
                    />
                    {isSearching && (
                        <div className="w-4 h-4 border-2 border-brand-500 border-t-transparent rounded-full animate-spin mr-2 flex-shrink-0" />
                    )}
                    <button onClick={onClose} className="p-1 hover:bg-surface-3 rounded-md transition-colors text-text-tertiary flex-shrink-0" aria-label="Close search">
                        <X size={18} />
                    </button>
                </div>

                {/* Type filter chips */}
                <div className="flex items-center gap-1.5 px-4 py-2 border-b border-border-subtle bg-surface-1 flex-wrap">
                    <span className="text-2xs text-text-disabled uppercase tracking-wider mr-1 flex-shrink-0">Filter:</span>
                    {FILTER_TYPES.map(({type, label}) => {
                        const active = activeTypes.has(type)
                        return (
                            <button
                                key={type}
                                onClick={() => toggleType(type)}
                                className={clsx(
                                    'text-2xs font-semibold px-2 py-0.5 rounded-full border transition-all duration-fast',
                                    active
                                        ? 'bg-brand-500/20 text-brand-300 border-brand-500/40'
                                        : 'bg-transparent text-text-tertiary border-border-subtle hover:border-border-default hover:text-text-secondary'
                                )}
                            >
                                {label}
                            </button>
                        )
                    })}
                    {activeTypes.size > 0 && (
                        <button
                            onClick={() => setActiveTypes(new Set())}
                            className="text-2xs text-text-tertiary hover:text-semantic-error transition-colors ml-1"
                        >
                            Clear
                        </button>
                    )}
                </div>

                {/* Results list */}
                <div ref={listRef} className="flex-1 overflow-y-auto custom-scrollbar p-1">
                    {results.length > 0 ? (
                        <div className="space-y-0.5">
                            {results.map((res, idx) => {
                                const subflow = document?.subflows.find(s => s.id === res.subflowId)
                                return (
                                    <div
                                        key={`${res.blockId}-${idx}`}
                                        role="option"
                                        aria-selected={idx === activeIndex}
                                        data-active={idx === activeIndex}
                                        className={clsx(
                                            'group flex flex-col px-4 py-3 cursor-pointer rounded-lg transition-colors duration-fast border border-transparent',
                                            idx === activeIndex
                                                ? 'bg-surface-3 border-brand-500/20 shadow-sm'
                                                : 'hover:bg-surface-2'
                                        )}
                                        onClick={() => handleSelect(res)}
                                        onMouseEnter={() => setActiveIndex(idx)}
                                    >
                                        <div className="flex items-center gap-2 mb-1">
                                            <FileCode size={14} className="text-brand-400" />
                                            <span className="text-xs font-semibold text-text-secondary uppercase tracking-tight">
                                                {subflow?.name || 'Unknown Subflow'}
                                            </span>
                                            <ChevronRight size={12} className="text-text-disabled" />
                                            <span className="text-xs text-text-tertiary">
                                                {res.matchedField}
                                            </span>
                                        </div>
                                        <div className="text-sm text-text-primary font-medium mb-1">
                                            {highlightText(res.matchedText, res.highlights)}
                                        </div>
                                    </div>
                                )
                            })}
                        </div>
                    ) : query.length > 0 && !isSearching ? (
                        <div className="py-12 text-center">
                            <div className="text-sm text-text-tertiary">No matches found for "{query}"
                                {activeTypes.size > 0 && ' in selected types'}
                            </div>
                        </div>
                    ) : (
                        <div className="py-12 text-center text-text-disabled">
                            <div className="text-sm">Type to search across the entire flow...</div>
                            <div className="text-xs mt-1">Search properties, variables, and comments</div>
                        </div>
                    )}
                </div>

                {/* Footer */}
                <div className="px-4 py-2 bg-surface-2 border-t border-border-subtle flex items-center justify-between text-2xs text-text-tertiary uppercase tracking-widest font-semibold">
                    <span>{results.length >= 50 ? '50+ results (refine query)' : `${results.length} results`}</span>
                    <div className="flex gap-4">
                        <span><kbd className="bg-surface-3 px-1 rounded border border-border-default mr-1">↑↓</kbd> Navigate</span>
                        <span><kbd className="bg-surface-3 px-1 rounded border border-border-default mr-1">Enter</kbd> Open</span>
                        <span><kbd className="bg-surface-3 px-1 rounded border border-border-default mr-1">Esc</kbd> Close</span>
                    </div>
                </div>
            </div>
        </div>
    )
}

function highlightText(text: string, highlights?: {start: number; end: number}[]) {
    if (!highlights?.length) return text

    const sorted = [...highlights].sort((a, b) => a.start - b.start)
    const result: React.ReactNode[] = []
    let last = 0

    sorted.forEach((h, i) => {
        if (h.start > last) {
            result.push(text.slice(last, h.start))
        }
        result.push(
            <span key={i} className="bg-brand-500/30 text-brand-200 rounded-sm px-0.5">
                {text.slice(h.start, h.end)}
            </span>
        )
        last = h.end
    })

    if (last < text.length) {
        result.push(text.slice(last))
    }

    return result
}
