import {useTranslation} from 'react-i18next'
import {useState, useEffect, useMemo, useCallback} from 'react'
import clsx from 'clsx'
import {
  Download,
  HelpCircle,
  Maximize,
  Minimize,
  Minimize2,
  MoreHorizontal,
  Plus,
  Search,
  Trash2,
  Type,
  Wrench,
} from 'lucide-react'
import {Dropdown, Tooltip, useConfirm, type DropdownItem} from '@/components/shared'
import {shortcuts, formatShortcutParts} from '@/lib/shortcuts'
import {isAgentModel} from '@/lib/modelIds'
import {subscribeConnectionState, getEventConnectionState, type EventConnectionState} from '@/api/client'
import ModelPicker, {type PickerProvider} from './ModelPicker'
import type {ChatTextSize, ModelDetail} from '@/types'

interface Props {
  /** The configured providers and their models (the picker's list). */
  providers: PickerProvider[]
  /** The active thread's provider, model ('' = nothing usable) and the model's name. */
  provider: string
  model: string
  modelLabel: string
  /** The thread model's listing entry, when listed (kind, purpose, shared, tool support). */
  modelDetail?: ModelDetail
  /** Per provider id: the model its new chats start on (the picker's [DEFAULT]). */
  defaultModels: Record<string, string>
  /** A pick from the picker: pins the active thread (only it). */
  onPickModel: (providerId: string, model: ModelDetail) => void
  /** The listing scope's organisation (named by the Shared badge). */
  orgName?: string
  /** The picker's "Your project has models you haven't added" line. */
  reviewPrompt?: boolean
  /** No thread to pin a pick to (no flow open). */
  pickerDisabled?: boolean
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
  /** Receives the model picker's trigger (focus goes back to it when a notice is dismissed). */
  pickerTriggerRef?: React.MutableRefObject<HTMLButtonElement | null>
}

type DisplayState = 'ready' | 'streaming' | 'connecting' | 'reconnecting'

function toDisplayState(sse: EventConnectionState, isStreaming?: boolean): DisplayState {
  if (sse === 'reconnecting') return 'reconnecting'
  if (sse === 'connecting') return 'connecting'
  if (sse === 'open') return isStreaming ? 'streaming' : 'ready'
  return 'ready' // 'idle' — SSE not active between streams; backend is up
}

// The connection state used to own a full-width row that said "Ready" for the
// entire time nothing was happening. It is a dot on the model picker's
// trigger now: the only states worth a word are the ones the user can act
// on, and those are still announced through the trigger's title.
function StatusDot({state}: {state: DisplayState}) {
  return (
    <span
      aria-hidden="true"
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
// The model control is the ModelPicker (each thread keeps its own model); the
// overflow menu is the shared <Dropdown>, which provides roving DOM focus,
// Escape, outside-click, viewport clamping and focus restore.
export default function ChatHeader({
  providers,
  provider,
  model,
  modelLabel,
  modelDetail,
  defaultModels,
  onPickModel,
  orgName,
  reviewPrompt,
  pickerDisabled,
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
  pickerTriggerRef,
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
  const {confirm} = useConfirm()
  const [sseState, setSseState] = useState<EventConnectionState>(getEventConnectionState)

  useEffect(() => subscribeConnectionState(setSseState), [])

  const display = toDisplayState(sseState, isStreaming)

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
      // A Foundry agent always runs the tool loop with the tools its portal
      // definition carries — baki's flow tools are never sent and the
      // per-thread useTools flag is ignored — so an on/off toggle would be
      // inert. Offer an unavailable item that says so instead (F-39) — with
      // its reason, so it stays reachable by keyboard and the reason is read.
      // An agent is known by its listed kind (a registry entry is
      // "m/<id>"), or by the pre-registry "agent:" prefix before the listing
      // says.
      const isAgent = provider === 'azure-foundry' && isAgentModel(model, modelDetail)
      if (isAgent) {
        items.push({
          type: 'item',
          label: t('toolbar.toolsAgent'),
          icon: Wrench,
          disabledReason: t('toolbar.toolsAgentReason'),
          onSelect: onToggleTools,
        })
      } else {
        // Providers without native function calling (Copilot) still run tools:
        // the chat service falls back to the text-marker protocol
        // (runPromptToolLoop), so the toggle stays enabled and only says which
        // protocol it will use — the same hint the model list shows.
        const viaMarkers = modelDetail?.supportsTools === false
        items.push({
          type: 'item',
          label:
            (useTools ? t('toolbar.toolsOn') : t('toolbar.toolsOff')) +
            (viaMarkers ? ` · ${t('provider.noNativeTools')}` : ''),
          icon: Wrench,
          onSelect: onToggleTools,
        })
      }
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
    provider,
    model,
    modelDetail,
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
      {/* The model name comes first and is never hidden at narrow widths
          (the default chat width is 320 px): at any width the header says
          which model this thread talks to. */}
      <ModelPicker
        className="flex-1"
        providers={providers}
        provider={provider}
        model={model}
        label={modelLabel}
        modelDetail={modelDetail}
        defaults={defaultModels}
        onSelect={onPickModel}
        orgName={orgName}
        reviewPrompt={reviewPrompt}
        disabled={pickerDisabled}
        leading={<StatusDot state={display} />}
        title={t(`connection.${display}`)}
        triggerRef={pickerTriggerRef}
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
