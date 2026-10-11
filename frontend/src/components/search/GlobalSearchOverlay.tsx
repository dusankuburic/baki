import {useState, useCallback, useRef, useEffect} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Search, X, ChevronRight, FileCode} from 'lucide-react'
import {useFlowStore} from '@/stores/flowStore'
import {flowApi, libraryApi} from '@/api'
import {useDebouncedSearch} from '@/hooks/useDebouncedSearch'
import {useListNavigation} from '@/hooks/useListNavigation'
import {useDialogFocus} from '@/hooks/useDialogFocus'
import {toggleSetMember} from '@/lib/collections'
import {logger} from '@/lib/logger'
import type {SearchResult, BlockType} from '@/types'

type GlobalSearchOverlayProps = {
  isOpen: boolean
  onClose: () => void
}

type Scope = 'this-flow' | 'all-flows'

const FILTER_TYPES: {type: BlockType; labelKey: keyof typeof TYPE_LABEL_KEYS}[] = [
  {type: 'ACTION', labelKey: 'action'},
  {type: 'CONDITION', labelKey: 'condition'},
  {type: 'LOOP', labelKey: 'loop'},
  {type: 'SWITCH', labelKey: 'switch'},
  {type: 'VARIABLE', labelKey: 'variable'},
  {type: 'ERROR_HANDLER', labelKey: 'errorHandler'},
  {type: 'COMMENT', labelKey: 'comment'},
]

const TYPE_LABEL_KEYS = {
  action: 'overlay.types.action',
  condition: 'overlay.types.condition',
  loop: 'overlay.types.loop',
  switch: 'overlay.types.switch',
  variable: 'overlay.types.variable',
  errorHandler: 'overlay.types.errorHandler',
  comment: 'overlay.types.comment',
} as const

const LISTBOX_ID = 'global-search-listbox'
const optionId = (index: number) => `global-search-option-${index}`

export default function GlobalSearchOverlay({isOpen, onClose}: GlobalSearchOverlayProps) {
  const {t} = useTranslation('search')
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<SearchResult[]>([])
  const [isSearching, setIsSearching] = useState(false)
  const [activeTypes, setActiveTypes] = useState<Set<BlockType>>(new Set())
  const [scope, setScope] = useState<Scope>('this-flow')
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const overlayRef = useRef<HTMLDivElement>(null)
  useDialogFocus({isOpen, onClose, closeOnEsc: false, containerRef: overlayRef})
  const document = useFlowStore(s => s.document)
  const selectBlock = useFlowStore(s => s.selectBlock)
  const selectSubflow = useFlowStore(s => s.selectSubflow)
  const setDocument = useFlowStore(s => s.setDocument)

  const handleSelect = useCallback(
    async (result: SearchResult) => {
      if (scope === 'all-flows' && result.flowId && result.flowId !== document?.id) {
        try {
          const doc = await libraryApi.getContent(result.flowId)
          if (doc) setDocument(doc)
        } catch (err) {
          logger.warn('Cross-flow search: could not load flow', err)
        }
      }
      selectBlock(result.blockId)
      selectSubflow(result.subflowId)
      onClose()
    },
    [scope, document, setDocument, selectBlock, selectSubflow, onClose],
  )

  const {activeIndex, setActiveIndex, handleKeyDown} = useListNavigation({
    count: results.length,
    onSelect: i => {
      if (results[i]) void handleSelect(results[i])
    },
    onClose,
  })

  useEffect(() => {
    if (isOpen) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setQuery('')
      setResults([])
      setActiveIndex(0)
      setActiveTypes(new Set())
      setScope('this-flow')
      requestAnimationFrame(() => inputRef.current?.focus())
    }
  }, [isOpen, setActiveIndex])

  const toggleType = useCallback((type: BlockType) => {
    setActiveTypes(prev => toggleSetMember(prev, type))
  }, [])

  const searchReqIdRef = useRef(0)
  const handleSearch = useCallback(
    async (q: string, types: Set<BlockType>) => {
      if (!q) {
        setResults([])
        return
      }
      if (scope === 'this-flow' && !document) {
        setResults([])
        return
      }
      const reqId = ++searchReqIdRef.current
      setIsSearching(true)
      try {
        const query = {
          text: q,
          fuzzy: true,
          maxResults: 50,
          blockTypes: types.size > 0 ? Array.from(types) : undefined,
        }
        const res =
          scope === 'all-flows'
            ? await flowApi.searchLibrary(query)
            : await flowApi.searchFlow(document?.id ?? '', query)
        if (reqId !== searchReqIdRef.current) return
        setResults(res.results as SearchResult[])
        setActiveIndex(0)
      } catch (err) {
        if (reqId !== searchReqIdRef.current) return
        logger.warn('Global search failed:', err)
      } finally {
        if (reqId === searchReqIdRef.current) setIsSearching(false)
      }
    },
    [scope, document, setActiveIndex],
  )

  const {search: debouncedSearch} = useDebouncedSearch({
    onSearch: q => handleSearch(q, activeTypes),
  })

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    if (query) void handleSearch(query, activeTypes)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeTypes])

  useEffect(() => {
    const active = listRef.current?.querySelector('[data-active="true"]')
    active?.scrollIntoView({block: 'nearest'})
  }, [activeIndex])

  if (!isOpen) return null

  return (
    <div className="fixed inset-0 z-modal flex items-start justify-center pt-[15vh]" onClick={onClose}>
      <div className="absolute inset-0 bg-surface-overlay backdrop-blur-xs" />
      <div
        ref={overlayRef}
        className="relative w-full max-w-[720px] bg-surface-1 border border-border-default rounded-xl shadow-2xl overflow-hidden animate-palette flex flex-col max-h-[70vh]"
        onClick={e => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-label={t('overlay.dialogAria')}
      >
        <div className="flex items-center px-4 border-b border-border-subtle bg-surface-2">
          <Search className="text-text-tertiary mr-3 shrink-0" size={18} />
          <input
            ref={inputRef}
            type="text"
            value={query}
            onChange={e => {
              setQuery(e.target.value)
              debouncedSearch(e.target.value)
            }}
            onKeyDown={handleKeyDown}
            placeholder={t('overlay.placeholder')}
            className="flex-1 bg-transparent py-4 text-base outline-hidden text-text-primary placeholder:text-text-disabled"
            role="combobox"
            aria-expanded={results.length > 0}
            aria-controls={LISTBOX_ID}
            aria-autocomplete="list"
            aria-label={t('overlay.inputAria')}
            aria-activedescendant={activeIndex >= 0 && results[activeIndex] ? optionId(activeIndex) : undefined}
          />
          {isSearching && (
            <div className="w-4 h-4 border-2 border-brand-500 border-t-transparent rounded-full animate-spin mr-2 shrink-0" />
          )}
          <button
            onClick={onClose}
            className="p-1 hover:bg-surface-3 rounded-md transition-colors text-text-tertiary shrink-0"
            aria-label={t('overlay.closeAria')}
          >
            <X size={18} />
          </button>
        </div>

        <div className="flex items-center gap-1.5 px-4 py-2 border-b border-border-subtle bg-surface-1 flex-wrap">
          <div className="flex items-center rounded-lg border border-border-default overflow-hidden shrink-0">
            {(['this-flow', 'all-flows'] as Scope[]).map(s => (
              <button
                key={s}
                onClick={() => {
                  setScope(s)
                  if (query) void handleSearch(query, activeTypes)
                }}
                className={clsx(
                  'text-2xs font-semibold px-2.5 py-1 transition-colors',
                  scope === s
                    ? 'bg-brand-500 text-brand-foreground'
                    : 'bg-transparent text-text-tertiary hover:text-text-secondary',
                )}
              >
                {s === 'this-flow' ? t('overlay.scopeThisFlow') : t('overlay.scopeAllFlows')}
              </button>
            ))}
          </div>
          <span className="text-2xs text-text-disabled uppercase tracking-wider mr-1 shrink-0">
            {t('overlay.filterLabel')}
          </span>
          {FILTER_TYPES.map(({type, labelKey}) => {
            const active = activeTypes.has(type)
            return (
              <button
                key={type}
                onClick={() => toggleType(type)}
                className={clsx(
                  'text-2xs font-semibold px-2 py-0.5 rounded-full border transition-all duration-fast',
                  active
                    ? 'bg-brand-500/20 text-brand-300 border-brand-500/40'
                    : 'bg-transparent text-text-tertiary border-border-subtle hover:border-border-default hover:text-text-secondary',
                )}
              >
                {t(TYPE_LABEL_KEYS[labelKey])}
              </button>
            )
          })}
          {activeTypes.size > 0 && (
            <button
              onClick={() => setActiveTypes(new Set())}
              className="text-2xs text-text-tertiary hover:text-semantic-error transition-colors ml-1"
            >
              {t('overlay.clear')}
            </button>
          )}
        </div>

        <div ref={listRef} className="flex-1 overflow-y-auto custom-scrollbar p-1">
          {results.length > 0 ? (
            <div className="space-y-0.5" role="listbox" id={LISTBOX_ID} aria-label={t('overlay.resultsAria')}>
              {results.map((res, idx) => {
                const subflow = document?.subflows.find(s => s.id === res.subflowId)
                const prev = idx > 0 ? results[idx - 1] : undefined
                const showFlowHeader = scope === 'all-flows' && res.flowId && res.flowId !== prev?.flowId
                return (
                  <div key={`${res.flowId ?? ''}-${res.blockId}-${idx}`} role="presentation">
                    {showFlowHeader && (
                      <div className="flex items-center gap-1.5 px-4 pt-3 pb-1 text-2xs font-bold uppercase tracking-wider text-brand-400">
                        <FileCode size={11} />
                        {res.flowName || res.flowId}
                      </div>
                    )}
                    <div
                      id={optionId(idx)}
                      role="option"
                      aria-selected={idx === activeIndex}
                      data-active={idx === activeIndex}
                      className={clsx(
                        'group flex flex-col px-4 py-3 cursor-pointer rounded-lg transition-colors duration-fast border border-transparent',
                        idx === activeIndex ? 'bg-surface-3 border-brand-500/20 shadow-sm' : 'hover:bg-surface-2',
                      )}
                      onClick={() => handleSelect(res)}
                      onMouseEnter={() => setActiveIndex(idx)}
                    >
                      <div className="flex items-center gap-2 mb-1">
                        <FileCode size={14} className="text-brand-400" />
                        <span className="text-xs font-semibold text-text-secondary uppercase tracking-tight">
                          {subflow?.name || t('overlay.unknownSubflow')}
                        </span>
                        <ChevronRight size={12} className="text-text-disabled" />
                        <span className="text-xs text-text-tertiary">{res.matchedField}</span>
                      </div>
                      <div className="text-sm text-text-primary font-medium mb-1">
                        {highlightText(res.matchedText, res.highlights)}
                      </div>
                    </div>
                  </div>
                )
              })}
            </div>
          ) : query.length > 0 && !isSearching ? (
            <div className="py-12 text-center">
              <div className="text-sm text-text-tertiary">
                {activeTypes.size > 0
                  ? t('overlay.noMatchesScoped', {query})
                  : t('overlay.noMatches', {query})}
              </div>
            </div>
          ) : (
            <div className="py-12 text-center text-text-disabled">
              <div className="text-sm">
                {scope === 'all-flows' ? t('overlay.hintAllFlows') : t('overlay.hintThisFlow')}
              </div>
              <div className="text-xs mt-1">{t('overlay.hintSub')}</div>
            </div>
          )}
        </div>

        <div className="px-4 py-2 bg-surface-2 border-t border-border-subtle flex items-center justify-between text-2xs text-text-tertiary uppercase tracking-widest font-semibold">
          <span>
            {results.length >= 50
              ? t('overlay.resultsCapped')
              : t('overlay.results', {count: results.length})}
          </span>
          <div className="flex gap-4">
            <span>
              <kbd className="bg-surface-3 px-1 rounded border border-border-default mr-1">↑↓</kbd>{' '}
              {t('overlay.navigate')}
            </span>
            <span>
              <kbd className="bg-surface-3 px-1 rounded border border-border-default mr-1">Enter</kbd>{' '}
              {t('overlay.open')}
            </span>
            <span>
              <kbd className="bg-surface-3 px-1 rounded border border-border-default mr-1">Esc</kbd>{' '}
              {t('overlay.close')}
            </span>
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
      </span>,
    )
    last = h.end
  })

  if (last < text.length) {
    result.push(text.slice(last))
  }

  return result
}
