import {useCallback, useEffect, useMemo, useRef} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {RefreshCw, FolderOpen} from 'lucide-react'
import {libraryApi} from '@/api'
import {Button, EmptyState, ErrorState, IconButton, useToast} from '@/components/shared'
import {useActiveSection} from '@/components/auth/useActiveSection'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useAnalyticsStore} from '@/stores/analyticsStore'
import {useFlowStore, beginDocLoad, isDocLoadCurrent} from '@/stores/flowStore'
import {useOrgStore} from '@/stores/orgStore'
import {useUIStore} from '@/stores/uiStore'
import type {FlowDocument} from '@/types'
import {AnalyticsFilterBar} from './AnalyticsFilterBar'
import {SkeletonAnalytics} from './SkeletonAnalytics'
import {useAnalyticsData} from './useAnalyticsData'
import {OverviewSection} from './sections/OverviewSection'
import {TrendSection} from './sections/TrendSection'
import {FindingsSection} from './sections/FindingsSection'
import {FlowsSection} from './sections/FlowsSection'
import {TriageSection} from './sections/TriageSection'
import {AiSection} from './sections/AiSection'
import {ActivitySection} from './sections/ActivitySection'
import {SecuritySection} from './sections/SecuritySection'

const SECTION_IDS = [
  'analytics-overview',
  'analytics-trend',
  'analytics-findings',
  'analytics-flows',
  'analytics-triage',
  'analytics-ai',
  'analytics-activity',
  'analytics-security',
] as const
type SectionKey = 'overview' | 'trend' | 'findings' | 'flows' | 'triage' | 'ai' | 'activity' | 'security'
const SECTION_KEYS: SectionKey[] = ['overview', 'trend', 'findings', 'flows', 'triage', 'ai', 'activity', 'security']

// The filterable Analytics page: one filter row, a section nav, and eight
// sections computed for the same window and scope. Refetches keep the
// previous charts on screen at reduced opacity; a failed refetch keeps them
// too, with a notice. The layout answers to the main pane's width (the
// `analytics` container in index.css).
export default function CloudAnalyticsPage() {
  const {t} = useTranslation('dashboard')
  const {data, error, isLoading, isRefetching, retry} = useAnalyticsData()
  const isAnalyzing = useAnalysisStore(s => s.isAnalyzing)
  const consumeSection = useAnalyticsStore(s => s.consumeSection)
  const pendingSection = useAnalyticsStore(s => s.pendingSection)
  const filters = useAnalyticsStore(s => s.filters)
  const setScope = useAnalyticsStore(s => s.setScope)
  const organisations = useOrgStore(s => s.organisations)
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const setDocument = useFlowStore(s => s.setDocument)
  const toast = useToast()
  const toastRef = useRef(toast)
  useEffect(() => {
    toastRef.current = toast
  })
  const {active, jumpTo} = useActiveSection(SECTION_IDS)

  // An org the caller no longer belongs to falls back to every scope.
  useEffect(() => {
    if (filters.scope === 'org' && organisations.length > 0 && !organisations.some(o => o.id === filters.orgId)) {
      setScope('all')
    }
  }, [filters.scope, filters.orgId, organisations, setScope])

  // Home's "Details" asked for a section: scroll there once the page exists.
  useEffect(() => {
    if (!data || !pendingSection) return
    const id = consumeSection()
    if (id) requestAnimationFrame(() => jumpTo(id))
  }, [data, pendingSection, consumeSection, jumpTo])

  const openFlow = useCallback(
    async (id: string) => {
      const gen = beginDocLoad()
      try {
        const full = await libraryApi.getContent(id)
        if (!isDocLoadCurrent(gen)) return
        setDocument(full as FlowDocument)
        useFlowStore.setState({libraryFlowId: id, libraryVersion: 0})
        setMainPaneView('block')
      } catch (e) {
        if (!isDocLoadCurrent(gen)) return
        toastRef.current.error(t('home.openFailed'), {
          description: e instanceof Error ? e.message : t('common.unknownError'),
        })
      }
    },
    [setDocument, setMainPaneView, t],
  )

  const partialLabels = useMemo(
    () =>
      (data?.partial ?? [])
        .map(key => (SECTION_KEYS.includes(key as SectionKey) ? t(`analytics.sections.${key as SectionKey}`) : key))
        .join(', '),
    [data, t],
  )

  return (
    <div className="analytics-page h-full w-full overflow-y-auto bg-surface-1">
      <div className="analytics-layout">
        <header className="mb-4 flex items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="text-2xl font-bold text-text-primary">{t('analytics.title')}</h1>
              {isAnalyzing && (
                <span className="inline-flex animate-pulse items-center gap-1.5 rounded-full bg-brand-500/10 px-2 py-0.5 text-xs text-brand-400">
                  <span className="h-1.5 w-1.5 rounded-full bg-brand-400" aria-hidden="true" />
                  {t('common.analyzing')}
                </span>
              )}
            </div>
            <p className="mt-0.5 text-sm text-text-tertiary">{t('analytics.subtitle')}</p>
          </div>
          <IconButton icon={RefreshCw} size="sm" label={t('common.refresh')} onClick={retry} />
        </header>

        <AnalyticsFilterBar
          facets={data?.facets ?? null}
          applied={data?.applied ?? null}
          range={data?.range ?? null}
          busy={isRefetching}
        />
        <div
          className={clsx('analytics-progress', isRefetching && 'is-active')}
          role="progressbar"
          aria-label={t('analytics.updating')}
          aria-hidden={isRefetching ? undefined : true}
        />

        {error && data && (
          <div
            role="alert"
            className="mb-4 flex items-center justify-between gap-3 rounded-lg border border-semantic-error/30 bg-semantic-error/10 px-3 py-2 text-sm text-semantic-error"
          >
            <span className="min-w-0">{error}</span>
            <Button size="sm" variant="ghost" onClick={retry}>
              {t('common.retry')}
            </Button>
          </div>
        )}
        {data && data.partial.length > 0 && (
          <div
            role="status"
            className="mb-4 rounded-lg border border-semantic-warning/30 bg-semantic-warning/10 px-3 py-2 text-xs text-text-secondary"
          >
            {t('analytics.partial', {sections: partialLabels})}
          </div>
        )}

        {isLoading ? (
          <div role="status" aria-label={t('analytics.loading')}>
            <SkeletonAnalytics />
          </div>
        ) : !data ? (
          <ErrorState
            title={t('analytics.loadFailed')}
            message={error ?? ''}
            onRetry={retry}
            retryLabel={t('common.retry')}
          />
        ) : data.summary.flowsInScope === 0 && data.facets.flows.length === 0 ? (
          <EmptyState
            icon={FolderOpen}
            title={t('analytics.emptyTitle')}
            description={t('analytics.emptyBody')}
            action={
              <Button size="sm" onClick={() => setMainPaneView('library')}>
                {t('common.openLibrary')}
              </Button>
            }
          />
        ) : (
          <div className="analytics-body">
            <nav className="analytics-nav" aria-label={t('analytics.nav')}>
              {SECTION_IDS.map((id, i) => (
                <button
                  key={id}
                  type="button"
                  onClick={() => jumpTo(id)}
                  aria-current={active === id ? 'location' : undefined}
                  className={clsx(
                    'analytics-nav-item rounded-md px-2.5 py-1.5 text-xs font-medium transition-colors duration-fast',
                    active === id
                      ? 'bg-brand-500/10 text-brand-400'
                      : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary',
                  )}
                >
                  {t(`analytics.sections.${SECTION_KEYS[i]}`)}
                </button>
              ))}
            </nav>
            <div className="analytics-content">
              <OverviewSection data={data} />
              <TrendSection data={data} refetching={isRefetching} />
              <FindingsSection data={data} refetching={isRefetching} />
              <FlowsSection data={data} refetching={isRefetching} onOpenFlow={openFlow} />
              <TriageSection data={data} refetching={isRefetching} />
              <AiSection data={data} refetching={isRefetching} />
              <ActivitySection data={data} refetching={isRefetching} />
              <SecuritySection data={data} />
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
