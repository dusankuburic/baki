import {getErrorMessage} from '@/lib/getErrorMessage'
import {lazy, Suspense, useCallback, useEffect, useRef, useState, type ReactNode} from 'react'
import {useTranslation} from 'react-i18next'
import {ArrowRight} from 'lucide-react'
import {dashboardApi, libraryApi} from '@/api'
import {logger} from '@/lib/logger'
import {useToast, ErrorState} from '@/components/shared'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore, beginDocLoad, isDocLoadCurrent} from '@/stores/flowStore'
import {useOrgStore} from '@/stores/orgStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useAnalyticsStore} from '@/stores/analyticsStore'
import type {DashboardHomeData, FlowDocument} from '@/types'
import {HomeKpiRow} from './home/HomeKpiRow'
import {RecentFlowsCard} from './home/RecentFlowsCard'
import {ActivityFeedCard} from './home/ActivityFeedCard'
import {SecurityPostureCard} from './home/SecurityPostureCard'
import {SkeletonCard, SkeletonDashboard} from './home/SkeletonDashboard'

// The recharts-backed cards arrive with the charts chunk (~120 kB gz), one
// hop after this one; the cards above (KPI tiles, recent flows, activity,
// security) import only ChartCard/StatTile and paint with this chunk. Each
// chart card sits in its own Suspense with a skeleton holding its grid slot,
// so a slow charts chunk never blanks the content that is already here.
const chartCards = () => import('./home/HomeChartCards')
const HealthTrendCard = lazy(() => chartCards().then(m => ({default: m.HealthTrendCard})))
const FindingsByCategoryCard = lazy(() => chartCards().then(m => ({default: m.FindingsByCategoryCard})))
const FindingsByRuleCard = lazy(() => chartCards().then(m => ({default: m.FindingsByRuleCard})))
const TriageCard = lazy(() => chartCards().then(m => ({default: m.TriageCard})))
const AiUsageCard = lazy(() => chartCards().then(m => ({default: m.AiUsageCard})))
const HealthDistributionCard = lazy(() => chartCards().then(m => ({default: m.HealthDistributionCard})))
const FlowComplexityCard = lazy(() => chartCards().then(m => ({default: m.FlowComplexityCard})))

// bodyHeight mirrors the front-end chart's height (see the card) so the grid
// row keeps its size while the charts chunk loads.
function ChartSlot({span, bodyHeight, children}: {span: string; bodyHeight?: number; children: ReactNode}) {
  return <Suspense fallback={<SkeletonCard className={span} bodyHeight={bodyHeight} />}>{children}</Suspense>
}

// Home is the at-a-glance overview: the headline numbers and one card per
// theme, each with a "Details" link into the section of the Analytics page
// that expands on it. The layout answers to the main pane's width (the
// `home` container in index.css), not the viewport's.
export default function HomeDashboard() {
  const {t} = useTranslation('dashboard')
  const [data, setData] = useState<DashboardHomeData | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const setDocument = useFlowStore(s => s.setDocument)
  const activeOrg = useOrgStore(s => s.organisations.find(o => o.id === s.activeOrgId))
  const activeOrgId = useOrgStore(s => s.activeOrgId)
  // Session analytics accumulate across single-file loads (only opening a base
  // FOLDER resets them server-side); re-fetch when the open document changes so
  // the dashboard reflects the latest state either way.
  const docId = useFlowStore(s => s.document?.id ?? null)
  const toast = useToast()
  const isAnalyzing = useAnalysisStore(s => s.isAnalyzing)
  const requestSection = useAnalyticsStore(s => s.requestSection)

  const loadIdRef = useRef(0)
  const hasDataRef = useRef(false)
  const toastRef = useRef(toast)
  useEffect(() => {
    toastRef.current = toast
  })

  const load = useCallback(() => {
    loadIdRef.current++
    const myId = loadIdRef.current
    dashboardApi
      .getHome()
      .then(d => {
        if (myId === loadIdRef.current) {
          setData(d)
          hasDataRef.current = true
          setError(null)
        }
      })
      .catch(e => {
        if (myId !== loadIdRef.current) return
        logger.error('dashboard: load failed', e)
        if (hasDataRef.current) {
          toastRef.current.error(t('home.refreshFailed'), {
            description: getErrorMessage(e, t('common.unknownError')),
          })
        } else {
          setError(getErrorMessage(e, t('home.loadFailed')))
        }
      })
      .finally(() => {
        if (myId === loadIdRef.current) setLoading(false)
      })
  }, [t])

  const retry = useCallback(() => {
    setLoading(true)
    setError(null)
    load()
  }, [load])

  useEffect(() => {
    hasDataRef.current = false
    // Enters the loading state for an async fetch (external system).
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setLoading(true)
    setError(null)
    load()
    return () => {
      // Incrementing the LIVE ref is the point — this invalidates whatever
      // request is in flight. The rule's suggested local copy would pin a
      // stale id and stop cancelling anything.
      // eslint-disable-next-line react-hooks/exhaustive-deps
      loadIdRef.current++
    }
  }, [load, activeOrgId, docId])

  // Re-fetch dashboard when analysis completes (isAnalyzing goes true→false).
  const wasAnalyzing = useRef(false)
  useEffect(() => {
    if (wasAnalyzing.current && !isAnalyzing) {
      load()
    }
    wasAnalyzing.current = isAnalyzing
  }, [isAnalyzing, load])

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
          description: getErrorMessage(e, t('common.unknownError')),
        })
      }
    },
    [setDocument, setMainPaneView, t],
  )

  // "Details" on a cloud card lands on that section of the Analytics page.
  const details = useCallback(
    (section: string) => () => {
      requestSection(section)
      setMainPaneView('dashboard')
    },
    [requestSection, setMainPaneView],
  )

  if (loading) {
    return (
      <div className="home-dashboard h-full w-full overflow-y-auto bg-surface-1">
        <SkeletonDashboard />
      </div>
    )
  }

  if (error || !data) {
    return (
      <div className="home-dashboard h-full w-full overflow-y-auto bg-surface-1">
        <div className="home-layout">
          <ErrorState
            title={t('home.loadFailed')}
            message={error ?? ''}
            onRetry={retry}
            retryLabel={t('common.retry')}
          />
        </div>
      </div>
    )
  }

  const {
    greeting,
    overview,
    tokenUsage,
    recentFlows,
    findings,
    isCloud,
    healthTrend,
    costByProvider,
    ruleFrequency,
    activity,
    complexity,
    security,
    severityTrend,
    healthBuckets,
    fixability,
    workflow,
  } = data
  const orgName = greeting.activeOrgName || activeOrg?.name
  const cloudDetails = (section: string) => (isCloud ? details(section) : undefined)

  return (
    <div className="home-dashboard h-full w-full overflow-y-auto bg-surface-1">
      <div className="home-layout">
        <header className="home-header mb-5 flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="text-2xl font-bold text-text-primary">
                {t('home.greeting', {timeOfDay: t(`home.${timeOfDay()}`), name: greeting.userDisplayName})}
              </h1>
              {isAnalyzing && (
                <span className="inline-flex animate-pulse items-center gap-1.5 rounded-full bg-brand-500/10 px-2 py-0.5 text-xs text-brand-400">
                  <span className="h-1.5 w-1.5 rounded-full bg-brand-400" aria-hidden="true" />
                  {t('common.analyzing')}
                </span>
              )}
            </div>
            <p className="mt-0.5 text-sm text-text-tertiary">
              {orgName ? `${orgName} · ` : ''}
              {t('home.subtitle')}
              {!isCloud && ` ${t('home.subtitleLocal')}`}
            </p>
          </div>
          {isCloud && (
            <button
              type="button"
              onClick={details('analytics-overview')}
              className="inline-flex shrink-0 items-center gap-1.5 rounded-lg border border-border-default bg-surface-2 px-3 py-1.5 text-sm text-text-primary transition-colors duration-fast hover:bg-surface-3 focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/40"
            >
              {t('common.openAnalytics')}
              <ArrowRight size={14} aria-hidden="true" />
            </button>
          )}
        </header>

        <HomeKpiRow
          overview={overview}
          findings={findings}
          healthTrend={isCloud ? healthTrend : []}
          severityTrend={isCloud ? severityTrend : []}
          fixability={fixability}
          costByProvider={isCloud ? costByProvider : []}
          // The portfolio is an org-wide governance view (cloud only, the
          // palette hides it the same way); on desktop the tile used to route
          // to a view backed by GET /api/library/portfolio, which 500'd there.
          onViewFindings={isCloud ? () => setMainPaneView('portfolio') : undefined}
          onViewFlows={isCloud ? () => setMainPaneView('library') : undefined}
          className="mb-4"
        />

        <div className="home-grid">
          {isCloud ? (
            <>
              <ChartSlot span="home-span-8" bodyHeight={200}>
                <HealthTrendCard
                  data={healthTrend}
                  onDetails={cloudDetails('analytics-trend')}
                  className="home-span-8"
                />
              </ChartSlot>
              <ChartSlot span="home-span-4">
                <FindingsByCategoryCard
                  findings={findings}
                  onDetails={cloudDetails('analytics-findings')}
                  className="home-span-4"
                />
              </ChartSlot>
              <ChartSlot span="home-span-7">
                <FindingsByRuleCard
                  data={ruleFrequency}
                  onDetails={cloudDetails('analytics-findings')}
                  className="home-span-7"
                />
              </ChartSlot>
              <ChartSlot span="home-span-5">
                <TriageCard data={workflow} onDetails={cloudDetails('analytics-triage')} className="home-span-5" />
              </ChartSlot>
              <RecentFlowsCard flows={recentFlows} onOpen={openFlow} className="home-span-7" />
              <ActivityFeedCard
                data={activity}
                onDetails={cloudDetails('analytics-activity')}
                className="home-span-5"
              />
              <ChartSlot span="home-span-8" bodyHeight={170}>
                <AiUsageCard
                  tokens={tokenUsage}
                  cost={costByProvider}
                  isCloud
                  onDetails={cloudDetails('analytics-ai')}
                  className="home-span-8"
                />
              </ChartSlot>
              <SecurityPostureCard
                data={security}
                onDetails={cloudDetails('analytics-security')}
                className="home-span-4"
              />
            </>
          ) : (
            <>
              <ChartSlot span="home-span-5">
                <FindingsByCategoryCard findings={findings} className="home-span-5" />
              </ChartSlot>
              <ChartSlot span="home-span-7">
                <FindingsByRuleCard data={ruleFrequency} className="home-span-7" />
              </ChartSlot>
              <RecentFlowsCard flows={recentFlows} onOpen={openFlow} className="home-span-7" />
              <ChartSlot span="home-span-5" bodyHeight={180}>
                <HealthDistributionCard data={healthBuckets} className="home-span-5" />
              </ChartSlot>
              <ChartSlot span="home-span-12" bodyHeight={220}>
                <FlowComplexityCard
                  rows={complexity.map(c => ({
                    id: c.flowId,
                    name: c.flowName,
                    blocks: c.blockCount,
                    findings: c.findingCount,
                    health: c.healthScore,
                  }))}
                  className="home-span-12"
                />
              </ChartSlot>
            </>
          )}
        </div>
      </div>
    </div>
  )
}

function timeOfDay(): 'morning' | 'afternoon' | 'evening' {
  const h = new Date().getHours()
  if (h < 12) return 'morning'
  if (h < 18) return 'afternoon'
  return 'evening'
}
