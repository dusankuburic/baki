import {useState, useRef, useEffect, useLayoutEffect, useCallback} from 'react'
import clsx from 'clsx'
import {Check, ChevronDown, Settings, Zap} from 'lucide-react'
import type {ModelDetail, ProviderID} from '@/types'
import {useUIStore} from '@/stores/uiStore'
import {Portal} from '../shared'

interface ProviderSummary {
  id: ProviderID
  name: string
  configured: boolean
  authType?: string
}

const PROVIDER_COLORS: Record<string, string> = {
  claude: '#d4a574',
  openai: '#10a37f',
  gemini: '#4285f4',
  xai: '#f43f5e',
  glm: '#06b6d4',
  'github-models': '#8b5cf6',
  copilot: '#6e40c9',
  demo: '#64748b',
}

interface Props {
  providers: ProviderSummary[]
  selectedProvider: ProviderID
  onSelectProvider: (id: ProviderID) => void
  models: ModelDetail[]
  selectedModel: string
  onSelectModel: (modelId: string) => void
  demoRemaining?: number | null
  onExport?: () => void
  hasMessages?: boolean
}

export default function ConnectionPanel({
  providers,
  selectedProvider,
  onSelectProvider,
  models,
  selectedModel,
  onSelectModel,
  demoRemaining,
  onExport,
  hasMessages,
}: Props) {
  const [providerOpen, setProviderOpen] = useState(false)
  const [modelsOpen, setModelsOpen] = useState(false)
  const [provPos, setProvPos] = useState({top: 0, left: 0, width: 0})
  const [modPos, setModPos] = useState({top: 0, left: 0, width: 0})
  const provRef = useRef<HTMLDivElement>(null)
  const modRef = useRef<HTMLDivElement>(null)
  const provDropdownRef = useRef<HTMLDivElement>(null)
  const modDropdownRef = useRef<HTMLDivElement>(null)

  // Anchor a fixed-position dropdown under its trigger, clamped into the viewport
  // and flipped above the trigger when there isn't room below. Mirrors the logic
  // in shared/Dropdown.tsx. Returns null when the trigger isn't laid out yet.
  const computePos = useCallback((trigger: HTMLElement | null, menu: HTMLElement | null) => {
    if (!trigger) return null
    const t = trigger.getBoundingClientRect()
    if (t.width === 0 && t.height === 0) return null
    const menuH = menu?.getBoundingClientRect().height ?? 0
    const vw = window.innerWidth
    const vh = window.innerHeight
    const width = t.width
    let left = t.left
    if (left + width > vw - 8) left = vw - width - 8
    if (left < 8) left = 8
    let top = t.bottom + 4
    if (menuH > 0 && top + menuH > vh - 8) {
      top = t.top - menuH - 4 > 8 ? t.top - menuH - 4 : Math.max(8, vh - menuH - 8)
    }
    return {top, left, width}
  }, [])

  // While a dropdown is open, keep it pinned to its trigger every frame so panel
  // resizes / layout shifts can't strand it. Measured in a layout effect (before
  // paint) to avoid a flash at the previous position.
  useLayoutEffect(() => {
    if (!providerOpen && !modelsOpen) return
    const same = (a: {top: number; left: number; width: number}, b: {top: number; left: number; width: number}) =>
      a.top === b.top && a.left === b.left && a.width === b.width
    const measure = () => {
      if (providerOpen) {
        const p = computePos(provRef.current, provDropdownRef.current)
        if (p) setProvPos(prev => (same(prev, p) ? prev : p))
      }
      if (modelsOpen) {
        const m = computePos(modRef.current, modDropdownRef.current)
        if (m) setModPos(prev => (same(prev, m) ? prev : m))
      }
    }
    measure()
    let raf = 0
    const loop = () => {
      measure()
      raf = requestAnimationFrame(loop)
    }
    raf = requestAnimationFrame(loop)
    return () => cancelAnimationFrame(raf)
  }, [providerOpen, modelsOpen, computePos])

  useEffect(() => {
    const handler = (e: MouseEvent) => {
      const target = e.target as Node
      const inProv = provRef.current?.contains(target) || provDropdownRef.current?.contains(target)
      const inMod = modRef.current?.contains(target) || modDropdownRef.current?.contains(target)
      if (!inProv) setProviderOpen(false)
      if (!inMod) setModelsOpen(false)
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [])

  const setSettingsOpen = useUIStore(s => s.setSettingsOpen)
  const currentProv = providers.find(p => p.id === selectedProvider)
  const currentMod = models.find(m => m.id === selectedModel)
  const connectedCount = providers.filter(p => p.configured).length

  return (
    <div className="px-3 pt-2 space-y-2">
      <div className="flex items-center gap-2">
        <div ref={provRef} className="relative flex-1 min-w-0">
          <button
            aria-haspopup="listbox"
            aria-expanded={providerOpen}
            className={clsx(
              'flex items-center gap-2 w-full px-2.5 py-1.5 rounded-lg text-sm transition-colors border',
              providerOpen ? 'bg-surface-2 border-border-default' : 'hover:bg-surface-2 border-transparent',
            )}
            onClick={() => {
              setProviderOpen(!providerOpen)
              setModelsOpen(false)
            }}
          >
            <span
              className="w-2 h-2 rounded-full shrink-0"
              style={{
                backgroundColor: currentProv?.configured
                  ? (PROVIDER_COLORS[currentProv.id] ?? 'var(--success)')
                  : 'var(--error)',
              }}
            />
            <span className="truncate text-text-secondary font-medium">{currentProv?.name || 'Select provider'}</span>
            <ChevronDown
              size={13}
              className={clsx(
                'shrink-0 text-text-tertiary transition-transform duration-fast',
                providerOpen && 'rotate-180',
              )}
            />
          </button>
          {providerOpen && (
            <Portal>
              <div
                ref={provDropdownRef}
                className="fixed bg-surface-1 border border-border-default rounded-lg shadow-lg z-tooltip py-1 animate-fade-in"
                style={{top: provPos.top, left: provPos.left, width: provPos.width}}
              >
                {providers.map(p => (
                  <button
                    key={p.id}
                    className={clsx(
                      'flex items-center gap-2.5 px-3 py-2 text-sm w-full text-left hover:bg-surface-2 transition-colors',
                      p.id === selectedProvider && p.configured && 'text-brand-400',
                      !p.configured && 'opacity-60',
                    )}
                    onClick={() => {
                      if (p.configured) {
                        onSelectProvider(p.id)
                        setProviderOpen(false)
                      } else {
                        setProviderOpen(false)
                        setSettingsOpen(true)
                      }
                    }}
                  >
                    <span
                      className="w-2 h-2 rounded-full shrink-0"
                      style={{backgroundColor: p.configured ? (PROVIDER_COLORS[p.id] ?? 'var(--success)') : undefined}}
                    />
                    <span className="flex-1 truncate">{p.name}</span>
                    {p.id === selectedProvider && p.configured && <Check size={14} className="text-brand-400" />}
                    {!p.configured && <span className="text-2xs text-text-tertiary">Configure →</span>}
                  </button>
                ))}
                <div className="px-3 py-1.5 border-t border-border-subtle mt-1 flex items-center justify-between">
                  <span className="text-2xs text-text-tertiary">
                    {connectedCount} of {providers.length} configured
                  </span>
                  <button
                    className="flex items-center gap-1 text-2xs text-brand-400 hover:text-brand-300 transition-colors"
                    onClick={() => {
                      setProviderOpen(false)
                      setSettingsOpen(true)
                    }}
                  >
                    <Settings size={11} />
                    Manage
                  </button>
                </div>
              </div>
            </Portal>
          )}
        </div>

        {hasMessages && onExport && (
          <button
            className="p-1.5 rounded-lg hover:bg-surface-2 text-text-tertiary hover:text-text-secondary transition-colors shrink-0"
            onClick={onExport}
            title="Export conversation"
            aria-label="Export conversation"
          >
            <Zap size={14} />
          </button>
        )}
      </div>

      {demoRemaining !== null && (
        <div
          className={clsx(
            'flex items-center gap-2 px-2.5 py-1.5 rounded-lg border',
            demoRemaining !== undefined && demoRemaining <= 0
              ? 'bg-red-500/10 border-red-500/20'
              : demoRemaining !== undefined && demoRemaining <= 5
                ? 'bg-amber-500/10 border-amber-500/20'
                : 'bg-brand-500/10 border-brand-500/20',
          )}
        >
          <Zap
            size={11}
            className={clsx(
              'shrink-0',
              demoRemaining !== undefined && demoRemaining <= 0
                ? 'text-red-400'
                : demoRemaining !== undefined && demoRemaining <= 5
                  ? 'text-amber-400'
                  : 'text-brand-400',
            )}
          />
          <span
            className={clsx(
              'text-2xs',
              demoRemaining !== undefined && demoRemaining <= 0
                ? 'text-red-300'
                : demoRemaining !== undefined && demoRemaining <= 5
                  ? 'text-amber-300'
                  : 'text-brand-300',
            )}
          >
            {demoRemaining !== undefined && demoRemaining <= 0
              ? 'Demo limit reached'
              : `Demo: ${demoRemaining} remaining today`}
          </span>
        </div>
      )}

      {models.length > 1 && (
        <div ref={modRef} className="relative">
          <button
            aria-haspopup="listbox"
            aria-expanded={modelsOpen}
            className={clsx(
              'flex items-center gap-1.5 w-full px-2.5 py-1.5 rounded-lg text-xs transition-colors border',
              modelsOpen ? 'bg-surface-2 border-border-default' : 'hover:bg-surface-2 border-transparent',
            )}
            onClick={() => {
              setModelsOpen(!modelsOpen)
              setProviderOpen(false)
            }}
          >
            <span className="text-text-tertiary">Model:</span>
            <span className="truncate text-text-secondary font-medium">{currentMod?.displayName || selectedModel}</span>
            {currentMod && (
              <span className="text-2xs text-text-tertiary shrink-0">{currentMod.contextLimit / 1000}k</span>
            )}
            <ChevronDown
              size={12}
              className={clsx(
                'shrink-0 text-text-tertiary transition-transform duration-fast ml-auto',
                modelsOpen && 'rotate-180',
              )}
            />
          </button>
          {modelsOpen && (
            <Portal>
              <div
                ref={modDropdownRef}
                className="fixed bg-surface-1 border border-border-default rounded-lg shadow-lg z-tooltip py-1 animate-fade-in max-h-[240px] overflow-y-auto"
                style={{top: modPos.top, left: modPos.left, width: modPos.width}}
              >
                {models.map(m => (
                  <button
                    key={m.id}
                    className={clsx(
                      'flex items-center gap-3 px-3 py-2 w-full text-left hover:bg-surface-2 transition-colors',
                      m.id === selectedModel && 'bg-brand-500/5',
                    )}
                    onClick={() => {
                      onSelectModel(m.id)
                      setModelsOpen(false)
                    }}
                  >
                    <div className="flex-1 min-w-0">
                      <div
                        className={clsx(
                          'text-xs font-medium truncate',
                          m.id === selectedModel ? 'text-brand-400' : 'text-text-secondary',
                        )}
                      >
                        {m.displayName}
                      </div>
                      <div className="text-2xs text-text-tertiary">
                        {m.contextLimit / 1000}k context
                        {m.inputCostPerM > 0 && ` · $${m.inputCostPerM}/$${m.outputCostPerM} per 1M`}
                      </div>
                    </div>
                    {m.id === selectedModel && <Check size={13} className="text-brand-400 shrink-0" />}
                  </button>
                ))}
              </div>
            </Portal>
          )}
        </div>
      )}

      {models.length <= 1 && currentMod && (
        <div className="flex items-center gap-1.5 px-2.5 text-2xs text-text-tertiary">
          <span>Using</span>
          <span className="text-text-secondary font-medium">{currentMod.displayName}</span>
          <span>· {currentMod.contextLimit / 1000}k ctx</span>
        </div>
      )}
    </div>
  )
}
