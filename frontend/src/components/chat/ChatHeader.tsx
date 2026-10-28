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
  providers: PickerProvider[]
  provider: string
  model: string
  modelLabel: string
  modelDetail?: ModelDetail
  defaultModels: Record<string, string>
  onPickModel: (providerId: string, model: ModelDetail) => void
  orgName?: string
  reviewPrompt?: boolean
  pickerDisabled?: boolean
  isStreaming?: boolean
  messageCount: number
  useTools: boolean
  onToggleTools?: () => void
  onNewChat: () => void
  onClearContext?: () => void
  onCompact?: () => void
  onExport?: () => void
  onToggleSearch?: () => void
  searchActive?: boolean
  onShowHelp?: () => void
  isFocused?: boolean
  onToggleFocus?: () => void
  chatTextSize?: ChatTextSize
  onCycleTextSize?: () => void
  pickerTriggerRef?: React.MutableRefObject<HTMLButtonElement | null>
}

type DisplayState = 'ready' | 'streaming' | 'connecting' | 'reconnecting' | 'disconnected'

// Ready only while the live-updates stream is open: a reply needs it. 'idle' is no stream at all.
function toDisplayState(sse: EventConnectionState, isStreaming?: boolean): DisplayState {
  if (sse === 'reconnecting') return 'reconnecting'
  if (sse === 'connecting') return 'connecting'
  if (sse === 'open') return isStreaming ? 'streaming' : 'ready'
  return 'disconnected'
}

function StatusDot({state}: {state: DisplayState}) {
  return (
    <span
      aria-hidden="true"
      className={clsx(
        'w-1.5 h-1.5 rounded-full shrink-0',
        state === 'ready' && 'bg-success',
        state === 'streaming' && 'bg-brand-400 animate-pulse-soft shadow-[0_0_6px_var(--brand-400)]',
        (state === 'connecting' || state === 'reconnecting') && 'bg-warning animate-pulse-soft',
        state === 'disconnected' && 'bg-semantic-error',
      )}
    />
  )
}

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
