import {useTranslation} from 'react-i18next'
import {useEffect, useRef} from 'react'
import {Bell, X, CheckCheck, Trash2, ChevronRight, Eye, EyeOff} from 'lucide-react'
import clsx from 'clsx'
import {useGovernanceStore} from '@/stores/governanceStore'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {libraryApi} from '@/api/library'
import {isTauri} from '@/platform/guards'
import {sevChip} from '@/components/dashboard/SeverityChips'
import type {FlowDocument} from '@/types'
import type {GovernanceAlert} from '@/api/governance'
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
  const {t} = useTranslation('shell')
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
        title={t('alerts.bell')}
        aria-label={t('alerts.bell')}
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
  const {t} = useTranslation('shell')
  const alerts = useGovernanceStore(s => s.alerts)
  const loading = useGovernanceStore(s => s.loading)
  const dismiss = useGovernanceStore(s => s.dismiss)
  const clear = useGovernanceStore(s => s.clearDismissed)
  const showDismissed = useGovernanceStore(s => s.showDismissed)
  const setShowDismissed = useGovernanceStore(s => s.setShowDismissed)
  const closePanel = useGovernanceStore(s => s.closePanel)
  const setDocument = useFlowStore(s => s.setDocument)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const openSettings = useUIStore(s => s.openSettings)

  // Dismissed rows only reach the list while the toggle is on, so Clear is
  // offered exactly when there is something on screen for it to remove.
  const hasDismissed = showDismissed && alerts.some(a => a.dismissedAt)

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

  // A new_rules notice is about rules, not one flow: it opens the rule settings.
  const openRules = () => {
    closePanel()
    openSettings('analysis')
  }

  return (
    <div className="absolute right-0 top-7 w-80 max-h-96 flex flex-col bg-surface-1 border border-border-default rounded-md shadow-xl overflow-hidden z-modal animate-palette">
      <div className="flex items-center justify-between px-3 py-2 border-b border-border-subtle">
        <span className="text-xs font-medium text-text-secondary">{t('alerts.heading')}</span>
        <div className="flex items-center gap-1">
          <button
            onClick={() => void setShowDismissed(!showDismissed)}
            className="flex items-center gap-1 px-1.5 py-0.5 rounded text-2xs text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast"
            aria-pressed={showDismissed}
            title={showDismissed ? t('alerts.hideDismissed') : t('alerts.showDismissed')}
          >
            {showDismissed ? <EyeOff size={11} /> : <Eye size={11} />}{' '}
            {showDismissed ? t('alerts.hideDismissedShort') : t('alerts.showDismissedShort')}
          </button>
          {hasDismissed && (
            <button
              onClick={() => void clear()}
              className="flex items-center gap-1 px-1.5 py-0.5 rounded text-2xs text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast"
              title={t('alerts.clearDismissed')}
            >
              <Trash2 size={11} /> {t('alerts.clear')}
            </button>
          )}
          <button
            onClick={closePanel}
            className="w-5 h-5 flex items-center justify-center rounded text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast"
            aria-label={t('alerts.close')}
          >
            <X size={12} />
          </button>
        </div>
      </div>

      <div className="flex-1 overflow-y-auto">
        {loading ? (
          <div className="py-8 text-center text-xs text-text-tertiary">{t('alerts.loading')}</div>
        ) : alerts.length === 0 ? (
          <div className="py-10 text-center text-xs text-text-tertiary flex flex-col items-center gap-2">
            <CheckCheck size={20} className="text-text-disabled" />
            {t('alerts.empty')}
          </div>
        ) : (
          alerts.map(a => (
            <AlertRow
              key={a.id}
              alert={a}
              onDismiss={() => void dismiss(a.id)}
              onOpen={a.type === 'new_rules' ? openRules : () => void openFlow(a.flowId)}
            />
          ))
        )}
      </div>
    </div>
  )
}

// How many rule ids a notice row shows before "+N more".
const NOTICE_RULES_SHOWN = 4

function AlertRow({alert, onDismiss, onOpen}: {alert: GovernanceAlert; onDismiss: () => void; onOpen: () => void}) {
  const {t} = useTranslation('shell')
  const isRegression = alert.type === 'health_regression'
  const isNotice = alert.type === 'new_rules'
  // Personal alert types get a friendlier chip than the raw severity: an
  // assignment is an action item, not an error state.
  const chip = isNotice
    ? t('alerts.newRules.chip')
    : isRegression
      ? 'health'
      : alert.type === 'finding_assigned'
        ? 'assigned'
        : alert.type === 'comment_added'
          ? 'comment'
          : alert.severity
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
            alert.type === 'finding_assigned' || alert.type === 'comment_added' || isNotice
              ? 'bg-brand-500/10 text-brand-400'
              : (sevChip[alert.severity] ?? sevChip.warning),
          )}
        >
          {chip}
        </span>
        <button
          className="flex-1 text-left min-w-0"
          onClick={onOpen}
          title={isNotice ? t('alerts.newRules.open') : undefined}
        >
          <div className="text-xs text-text-primary line-clamp-2">{alert.title}</div>
          {isNotice ? (
            <NoticeRules alert={alert} />
          ) : (
            alert.message && <div className="text-2xs text-text-tertiary mt-0.5 line-clamp-2">{alert.message}</div>
          )}
          <div className="flex items-center gap-1 mt-1 text-2xs text-text-disabled">
            <span className="truncate max-w-[140px]">
              {isNotice ? <NoticeScope scope={alert.scope} /> : alert.flowName || alert.flowId}
            </span>
            <ChevronRight size={9} />
            <span>{timeAgo(alert.createdAt)}</span>
          </div>
        </button>
        <button
          onClick={onDismiss}
          className="opacity-0 group-hover:opacity-100 w-5 h-5 flex items-center justify-center rounded text-text-tertiary hover:text-text-secondary hover:bg-surface-3 transition-colors duration-fast shrink-0"
          title={t('alerts.dismiss')}
          aria-label={t('alerts.dismissAlert')}
        >
          <X size={11} />
        </button>
      </div>
    </div>
  )
}

// The rules of a new_rules notice and where they report findings. The ids are
// data (rule ids), not copy.
function NoticeRules({alert}: {alert: GovernanceAlert}) {
  const {t} = useTranslation('shell')
  const ids = alert.ruleIds ?? []
  const more = ids.length - NOTICE_RULES_SHOWN
  return (
    <div className="mt-0.5">
      {ids.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {ids.slice(0, NOTICE_RULES_SHOWN).map(id => (
            <code key={id} className="px-1 rounded bg-surface-3 text-2xs text-text-secondary">
              {id}
            </code>
          ))}
          {more > 0 && <span className="text-2xs text-text-tertiary">{t('alerts.newRules.more', {count: more})}</span>}
        </div>
      )}
      {alert.flowCount != null && alert.flowCount > 0 && (
        <div className="text-2xs text-text-tertiary mt-0.5">{t('alerts.newRules.flows', {count: alert.flowCount})}</div>
      )}
    </div>
  )
}

// Where a notice's flows are: in place of an alert's flow name.
function NoticeScope({scope}: {scope: GovernanceAlert['scope']}) {
  const {t} = useTranslation('shell')
  switch (scope) {
    case 'org':
      return <>{t('alerts.newRules.scopeOrg')}</>
    case 'user':
      return <>{t('alerts.newRules.scopeUser')}</>
    default:
      return <>{t('alerts.newRules.scopeApp')}</>
  }
}
