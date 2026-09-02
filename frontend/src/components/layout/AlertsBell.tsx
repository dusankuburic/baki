import {useEffect, useRef} from 'react'
import {Bell, X, CheckCheck, Trash2, ChevronRight} from 'lucide-react'
import clsx from 'clsx'
import {useGovernanceStore} from '@/stores/governanceStore'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {libraryApi} from '@/api/library'
import {isTauri} from '@/platform/guards'
import {sevChip} from '@/components/dashboard/SeverityChips'
import type {FlowDocument} from '@/types'
import {logger} from '@/lib/logger'

// Notifications bell + dropdown panel for the in-app governance alerts inbox.
// Cloud-only (the scanner is cloud-only); hidden in desktop/Tauri mode. The
// unread badge polls on a slow interval; opening the panel loads the list and
// implicitly clears the badge (ack semantics).

function timeAgo(iso: string): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ''
  const s = Math.round((Date.now() - then) / 1000)
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

export default function AlertsBell() {
  // Desktop mode has no scanner → no alerts; hide the bell entirely.
  if (isTauri()) return null
  return <AlertsBellInner />
}

function AlertsBellInner() {
  const unreadCount = useGovernanceStore(s => s.unreadCount)
  const panelOpen = useGovernanceStore(s => s.panelOpen)
  const openPanel = useGovernanceStore(s => s.openPanel)
  const closePanel = useGovernanceStore(s => s.closePanel)
  const containerRef = useRef<HTMLDivElement>(null)

  // Outside-click + Esc to close.
  useEffect(() => {
    if (!panelOpen) return
    const onPointer = (e: MouseEvent) => {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        closePanel()
      }
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') closePanel()
    }
    document.addEventListener('mousedown', onPointer)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onPointer)
      document.removeEventListener('keydown', onKey)
    }
  }, [panelOpen, closePanel])

  return (
    <div className="relative" ref={containerRef}>
      <button
        onClick={() => (panelOpen ? closePanel() : openPanel())}
        className="relative w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast"
        title="Governance alerts"
        aria-label="Governance alerts"
        aria-expanded={panelOpen}
      >
        <Bell size={12} />
        {unreadCount > 0 && (
          <span className="absolute -top-0.5 -right-0.5 min-w-[14px] h-[14px] px-0.5 flex items-center justify-center rounded-full bg-semantic-error text-white text-2xs font-semibold leading-none">
            {unreadCount > 99 ? '99+' : unreadCount}
          </span>
        )}
      </button>
      {panelOpen && <AlertsPanel />}
    </div>
  )
}

function AlertsPanel() {
  const alerts = useGovernanceStore(s => s.alerts)
  const loading = useGovernanceStore(s => s.loading)
  const dismiss = useGovernanceStore(s => s.dismiss)
  const clear = useGovernanceStore(s => s.clearDismissed)
  const closePanel = useGovernanceStore(s => s.closePanel)
  const setDocument = useFlowStore(s => s.setDocument)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)

  const hasDismissed = alerts.some(a => a.dismissedAt)

  const openFlow = async (flowId: string) => {
    closePanel()
    try {
      const fullDoc = await libraryApi.getContent(flowId)
      setDocument(fullDoc as FlowDocument)
      useFlowStore.setState({libraryFlowId: flowId})
      setMainPaneView('block')
    } catch (err) {
      logger.warn('governance: open flow from alert failed', err)
    }
  }

  return (
    <div className="absolute right-0 top-7 w-80 max-h-96 flex flex-col bg-surface-1 border border-border-default rounded-md shadow-xl overflow-hidden z-modal animate-palette">
      <div className="flex items-center justify-between px-3 py-2 border-b border-border-subtle">
        <span className="text-xs font-medium text-text-secondary">Governance alerts</span>
        <div className="flex items-center gap-1">
          {hasDismissed && (
            <button
              onClick={() => void clear()}
              className="flex items-center gap-1 px-1.5 py-0.5 rounded text-2xs text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast"
              title="Clear dismissed alerts"
            >
              <Trash2 size={11} /> Clear
            </button>
          )}
          <button
            onClick={closePanel}
            className="w-5 h-5 flex items-center justify-center rounded text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast"
            aria-label="Close alerts"
          >
            <X size={12} />
          </button>
        </div>
      </div>

      <div className="flex-1 overflow-y-auto">
        {loading ? (
          <div className="py-8 text-center text-xs text-text-tertiary">Loading…</div>
        ) : alerts.length === 0 ? (
          <div className="py-10 text-center text-xs text-text-tertiary flex flex-col items-center gap-2">
            <CheckCheck size={20} className="text-text-disabled" />
            No governance alerts
          </div>
        ) : (
          alerts.map(a => (
            <AlertRow
              key={a.id}
              alert={a}
              onDismiss={() => void dismiss(a.id)}
              onOpenFlow={() => void openFlow(a.flowId)}
            />
          ))
        )}
      </div>
    </div>
  )
}

function AlertRow({
  alert,
  onDismiss,
  onOpenFlow,
}: {
  alert: import('@/api/governance').GovernanceAlert
  onDismiss: () => void
  onOpenFlow: () => void
}) {
  const isRegression = alert.type === 'health_regression'
  // Personal alert types get a friendlier chip than the raw severity: an
  // assignment is an action item, not an error state.
  const chip = isRegression ? 'health' : alert.type === 'finding_assigned' ? 'assigned' : alert.type === 'comment_added' ? 'comment' : alert.severity
  return (
    <div
      className={clsx(
        'group px-3 py-2 border-b border-border-subtle last:border-b-0 hover:bg-surface-2 transition-colors duration-fast',
        alert.dismissedAt && 'opacity-50',
      )}
    >
      <div className="flex items-start gap-2">
        <span
          className={clsx(
            'mt-0.5 px-1 py-0.5 rounded text-2xs font-medium shrink-0',
            alert.type === 'finding_assigned' || alert.type === 'comment_added'
              ? 'bg-brand-500/10 text-brand-400'
              : sevChip[alert.severity] ?? sevChip.warning,
          )}
        >
          {chip}
        </span>
        <button className="flex-1 text-left" onClick={onOpenFlow}>
          <div className="text-xs text-text-primary line-clamp-2">{alert.title}</div>
          {alert.message && <div className="text-2xs text-text-tertiary mt-0.5 line-clamp-2">{alert.message}</div>}
          <div className="flex items-center gap-1 mt-1 text-2xs text-text-disabled">
            <span className="truncate max-w-[140px]">{alert.flowName || alert.flowId}</span>
            <ChevronRight size={9} />
            <span>{timeAgo(alert.createdAt)}</span>
          </div>
        </button>
        <button
          onClick={onDismiss}
          className="opacity-0 group-hover:opacity-100 w-5 h-5 flex items-center justify-center rounded text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast shrink-0"
          title="Dismiss"
          aria-label="Dismiss alert"
        >
          <X size={11} />
        </button>
      </div>
    </div>
  )
}
