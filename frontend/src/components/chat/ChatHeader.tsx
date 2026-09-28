import {useTranslation} from 'react-i18next'
import {useState, useEffect, useMemo, useCallback} from 'react'
import clsx from 'clsx'
import {
  Check,
  ChevronDown,
  Download,
  HelpCircle,
  Maximize,
  Minimize,
  Minimize2,
  MoreHorizontal,
  Plus,
  Search,
  Settings,
  Trash2,
  Type,
  Wrench,
} from 'lucide-react'
import {Dropdown, Tooltip, useConfirm, type DropdownItem} from '@/components/shared'
import {shortcuts, formatShortcutParts} from '@/lib/shortcuts'
import {useUIStore} from '@/stores/uiStore'
import {subscribeConnectionState, getEventConnectionState, type EventConnectionState} from '@/api/client'
import type {ChatTextSize, ModelDetail, ProviderID} from '@/types'

interface ProviderSummary {
  id: ProviderID
  name: string
  configured: boolean
  authType?: string
  /** Credentials come from the active organisation (badged in the chip). */
  shared?: boolean
}

interface Props {
  providers: ProviderSummary[]
  selectedProvider: ProviderID
  onSelectProvider: (id: ProviderID) => void
  models: ModelDetail[]
  selectedModel: string
  onSelectModel: (modelId: string) => void
  isStreaming?: boolean
  messageCount: number
  useTools: boolean
  onToggleTools?: () => void
  onNewChat: () => void
  // Clear / Compact rewrite the server's copy of the thread, which needs edit
  // rights on the flow: omit them (read-only flow) and the actions are hidden.
  onClearContext?: () => void
  onCompact?: () => void
  onExport?: () => void
  onToggleSearch?: () => void
  searchActive?: boolean
  onShowHelp?: () => void
  // Focus mode (the full-window reading layout); omitted where it can't be
  // offered (mobile drawer, standalone renders).
  isFocused?: boolean
  onToggleFocus?: () => void
  chatTextSize?: ChatTextSize
  onCycleTextSize?: () => void
}

type DisplayState = 'ready' | 'streaming' | 'connecting' | 'reconnecting'

function toDisplayState(sse: EventConnectionState, isStreaming?: boolean): DisplayState {
  if (sse === 'reconnecting') return 'reconnecting'
  if (sse === 'connecting') return 'connecting'
  if (sse === 'open') return isStreaming ? 'streaming' : 'ready'
  return 'ready' // 'idle' — SSE not active between streams; backend is up
}

// The connection state used to own a full-width row that said "Ready" for the
// entire time nothing was happening. It is a dot on the provider chip now: the
// only states worth a word are the ones the user can act on, and those are
// still announced through the chip's title.
function StatusDot({state}: {state: DisplayState}) {
  return (
    <span
      className={clsx(
        'w-1.5 h-1.5 rounded-full shrink-0',
        state === 'ready' && 'bg-success',
        state === 'streaming' && 'bg-brand-400 animate-pulse-soft shadow-[0_0_6px_var(--brand-400)]',
        (state === 'connecting' || state === 'reconnecting') && 'bg-warning animate-pulse-soft',
      )}
    />
  )
}

// ChatHeader is the panel's single row of chrome. It replaces what used to be
// three stacked full-width rows (provider selector, model selector, connection
// status) plus a fourth action toolbar — roughly 110px of vertical space in a
// column that is 320px wide by default.
//
// Both menus are the shared <Dropdown>: it already provides roving DOM focus,
// Escape, outside-click, viewport clamping and focus restore, which the two
// hand-rolled chat dropdowns did not (and one of which ran an uncapped
// requestAnimationFrame measuring loop for as long as it was open).
export default function ChatHeader({
  providers,
  selectedProvider,
  onSelectProvider,
  models,
  selectedModel,
  onSelectModel,
  isStreaming,
  messageCount,
  useTools,
  onToggleTools,
  onNewChat,
  onClearContext,
  onCompact,
  onExport,
  onToggleSearch,
  searchActive,
  onShowHelp,
  isFocused,
  onToggleFocus,
  chatTextSize,
  onCycleTextSize,
}: Props) {
  const {t} = useTranslation('chat')
  const focusLabel = isFocused ? t('panel.exitFocus') : t('panel.focus')
  // The registry is the single source for the combo shown in the tooltip.
  const focusShortcut = useMemo(() => {
    const keys = shortcuts.find(s => s.id === 'ai.focus')?.keys
    return keys
      ? formatShortcutParts(keys)
          .map(p => p.display)
          .join('+')
      : undefined
  }, [])
  const openSettings = useUIStore(s => s.openSettings)
  const {confirm} = useConfirm()
  const [sseState, setSseState] = useState<EventConnectionState>(getEventConnectionState)

  useEffect(() => subscribeConnectionState(setSseState), [])

  const display = toDisplayState(sseState, isStreaming)
  const currentProvider = providers.find(p => p.id === selectedProvider)
  const currentModel = models.find(m => m.id === selectedModel)
  const connectedCount = providers.filter(p => p.configured).length

  // Destructive actions route through the shared confirm dialog. The old
  // inline "Cancel / Confirm" strip mutated the toolbar's width mid-decision
  // and silently withdrew the offer after five seconds.
  const confirmThen = useCallback(
    (kind: 'clear' | 'compact', run: () => void) => {
      void confirm({
        title: t(kind === 'clear' ? 'toolbar.confirmClearTitle' : 'toolbar.confirmCompactTitle'),
        message: t(kind === 'clear' ? 'toolbar.confirmClear' : 'toolbar.confirmCompact'),
        confirmLabel: t(kind === 'clear' ? 'toolbar.clear' : 'toolbar.compact'),
        danger: kind === 'clear',
      }).then(ok => {
        if (ok) run()
      })
    },
    [confirm, t],
  )

  const connectionItems = useMemo<DropdownItem[]>(() => {
    const items: DropdownItem[] = [
      {
        type: 'group',
        label: t('provider.groupLabel'),
        items: providers.map(p => ({
          type: 'item' as const,
          label: p.configured
            ? p.shared
              ? `${p.name} · ${t('provider.shared')}`
              : p.name
            : t('provider.notConfigured', {name: p.name}),
          icon: p.id === selectedProvider && p.configured ? Check : undefined,
          onSelect: () => {
            if (p.configured) onSelectProvider(p.id)
            else openSettings('accounts')
          },
        })),
      },
    ]
    if (models.length > 0) {
      items.push({type: 'separator'})
      items.push({
        type: 'group',
        label: t('provider.model'),
        items: models.map(m => ({
          type: 'item' as const,
          label:
            `${m.displayName} · ${Math.round(m.contextLimit / 1000)}k` +
            (m.inputCostPerM > 0 ? ` · $${m.inputCostPerM}/$${m.outputCostPerM}` : '') +
            (m.supportsTools === false ? ` · ${t('provider.noNativeTools')}` : ''),
          icon: m.id === selectedModel ? Check : undefined,
          onSelect: () => onSelectModel(m.id),
        })),
      })
    }
    items.push({type: 'separator'})
    items.push({
      type: 'item',
      label: t('provider.manage', {connected: connectedCount, total: providers.length}),
      icon: Settings,
      onSelect: () => openSettings('accounts'),
    })
    return items
  }, [
    providers,
    selectedProvider,
    onSelectProvider,
    models,
    selectedModel,
    onSelectModel,
    connectedCount,
    openSettings,
    t,
  ])

  const overflowItems = useMemo<DropdownItem[]>(() => {
    const items: DropdownItem[] = [{type: 'item', label: t('toolbar.newChat'), icon: Plus, onSelect: onNewChat}]
    if (onToggleSearch) {
      items.push({
        type: 'item',
        label: searchActive ? t('toolbar.searchClose') : t('toolbar.search'),
        icon: Search,
        onSelect: onToggleSearch,
        disabled: messageCount === 0,
      })
    }
    if (onToggleTools) {
      // Providers without native function calling (Copilot) still run tools:
      // the chat service falls back to the text-marker protocol
      // (runPromptToolLoop), so the toggle stays enabled and only says which
      // protocol it will use — the same suffix the model list shows.
      const viaMarkers = currentModel?.supportsTools === false
      items.push({
        type: 'item',
        label:
          (useTools ? t('toolbar.toolsOn') : t('toolbar.toolsOff')) +
          (viaMarkers ? ` · ${t('provider.noNativeTools')}` : ''),
        icon: Wrench,
        onSelect: onToggleTools,
      })
    }
    if (onExport) {
      items.push({
        type: 'item',
        label: t('provider.exportConversation'),
        icon: Download,
        onSelect: onExport,
        disabled: messageCount === 0,
      })
    }
    if (onToggleFocus) {
      items.push({
        type: 'item',
        label: focusLabel,
        icon: isFocused ? Minimize : Maximize,
        shortcut: focusShortcut,
        onSelect: onToggleFocus,
      })
    }
    if (onCycleTextSize && chatTextSize) {
      items.push({
        type: 'item',
        label: t('toolbar.textSize', {size: t(`textSize.${chatTextSize}`)}),
        icon: Type,
        onSelect: onCycleTextSize,
      })
    }
    if (onShowHelp) {
      items.push({type: 'item', label: t('toolbar.help'), icon: HelpCircle, onSelect: onShowHelp})
    }
    // Disabled while this thread streams: the reply in flight would land in
    // a thread that was just cleared or compacted.
    if (onCompact || onClearContext) items.push({type: 'separator'})
    if (onCompact) {
      items.push({
        type: 'item',
        label: t('toolbar.compact'),
        icon: Minimize2,
        disabled: messageCount === 0 || !!isStreaming,
        onSelect: () => confirmThen('compact', onCompact),
      })
    }
    if (onClearContext) {
      items.push({
        type: 'item',
        label: t('toolbar.clear'),
        icon: Trash2,
        danger: true,
        disabled: messageCount === 0 || !!isStreaming,
        onSelect: () => confirmThen('clear', onClearContext),
      })
    }
    return items
  }, [
    t,
    onNewChat,
    onToggleSearch,
    searchActive,
    messageCount,
    isStreaming,
    onToggleTools,
    useTools,
    currentModel,
    onExport,
    onToggleFocus,
    isFocused,
    focusLabel,
    focusShortcut,
    chatTextSize,
    onCycleTextSize,
    onShowHelp,
    confirmThen,
    onCompact,
    onClearContext,
  ])

  return (
    <div className="chat-bar flex items-center gap-1 py-1.5 border-b border-border-subtle">
      <Dropdown
        className="min-w-0 flex-1"
        items={connectionItems}
        trigger={
          <button
            type="button"
            className="flex items-center gap-1.5 w-full min-w-0 px-2 py-1 rounded-lg text-xs hover:bg-surface-2 transition-colors"
            title={t(`connection.${display}`)}
          >
            <StatusDot state={display} />
            <span className="truncate font-medium text-text-secondary">
              {currentProvider?.name || t('provider.select')}
            </span>
            {currentProvider?.shared && (
              <span
                className="shrink-0 px-1 py-px rounded text-2xs font-semibold uppercase tracking-wider bg-brand-500/15 text-brand-400"
                // The tooltip is mouse-only; the name carries the meaning
                // ("using your organisation's credentials") to screen readers.
                role="img"
                aria-label={t('provider.sharedTitle')}
                title={t('provider.sharedTitle')}
              >
                {t('provider.shared')}
              </span>
            )}
            {currentModel && (
              // Hidden by the container query below ~340px: at that width the
              // provider name alone is all that fits without truncating both.
              <span className="cq-model truncate text-text-tertiary">· {currentModel.displayName}</span>
            )}
            <ChevronDown size={12} className="shrink-0 text-text-tertiary" />
          </button>
        }
      />

      {messageCount > 0 && (
        <span className="cq-count shrink-0 text-2xs text-text-tertiary/60 tabular-nums px-1">{messageCount}</span>
      )}

      {onToggleFocus && (
        <Tooltip content={focusLabel} shortcut={focusShortcut}>
          <button
            type="button"
            onClick={onToggleFocus}
            aria-label={focusLabel}
            aria-pressed={!!isFocused}
            className="shrink-0 flex items-center justify-center w-6 h-6 rounded-md text-text-tertiary hover:text-text-secondary hover:bg-surface-2 transition-colors"
          >
            {isFocused ? <Minimize size={14} /> : <Maximize size={14} />}
          </button>
        </Tooltip>
      )}

      <Dropdown
        align="end"
        items={overflowItems}
        trigger={
          <button
            type="button"
            className="shrink-0 flex items-center justify-center w-6 h-6 rounded-md text-text-tertiary hover:text-text-secondary hover:bg-surface-2 transition-colors"
            aria-label={t('toolbar.moreActions')}
            title={t('toolbar.moreActions')}
          >
            <MoreHorizontal size={14} />
          </button>
        }
      />
    </div>
  )
}
