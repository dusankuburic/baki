import {useCallback, useEffect, useRef, useState} from 'react'
import {RefreshCw, AlertTriangle} from 'lucide-react'
import {dashboardApi, libraryApi} from '@/api'
import {logger} from '@/lib/logger'
import {useToast} from '@/components/shared'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore, beginDocLoad, isDocLoadCurrent} from '@/stores/flowStore'
import {useOrgStore} from '@/stores/orgStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import type {DashboardHomeData, FlowDocument} from '@/types'
import {KPIStripCard} from './home/KPIStripCard'
import {HealthTrendCard} from './home/HealthTrendCard'
import {CostBreakdownCard} from './home/CostBreakdownCard'
import {HealthGaugeCard} from './home/HealthGaugeCard'
import {AITokenUsageCard} from './home/AITokenUsageCard'
import {FindingsChartCard} from './home/FindingsChartCard'
import {RuleFrequencyCard} from './home/RuleFrequencyCard'
import {RecentFlowsCard} from './home/RecentFlowsCard'
import {ActivityFeedCard} from './home/ActivityFeedCard'
import {FlowComplexityCard} from './home/FlowComplexityCard'
import {SecurityPostureCard} from './home/SecurityPostureCard'
import {SeverityTrendCard} from './home/SeverityTrendCard'
import {ConfidenceDonutCard} from './home/ConfidenceDonutCard'
import {HealthDistributionCard} from './home/HealthDistributionCard'
import {FixabilityCard} from './home/FixabilityCard'
import {WorkflowFunnelCard} from './home/WorkflowFunnelCard'
import {SkeletonDashboard} from './home/SkeletonDashboard'

export default function HomeDashboard() {
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

  const loadIdRef = useRef(0)
  const hasDataRef = useRef(false)
  const toastRef = useRef(toast)
  toastRef.current = toast

  const load = useCallback(() => {
    loadIdRef.current++
    const myId = loadIdRef.current
    dashboardApi.getHome()
      .then(d => { if (myId === loadIdRef.current) { setData(d); hasDataRef.current = true; setError(null) } })
      .catch(e => {
        if (myId !== loadIdRef.current) return
        logger.error('dashboard: load failed', e)
        if (hasDataRef.current) {
          toastRef.current.error('Dashboard refresh failed', {
            description: e instanceof Error ? e.message : 'Unknown error',
          })
        } else {
          setError(e instanceof Error ? e.message : 'Failed to load dashboard')
        }
      })
      .finally(() => { if (myId === loadIdRef.current) setLoading(false) })
  }, [])

  const retry = useCallback(() => {
    setLoading(true)
    setError(null)
    load()
  }, [load])

  useEffect(() => {
    hasDataRef.current = false
    setLoading(true)
    setError(null)
    load()
    return () => { loadIdRef.current++ }
  }, [load, activeOrgId, docId])

  // Re-fetch dashboard when analysis completes (isAnalyzing goes true→false).
  const wasAnalyzing = useRef(false)
  useEffect(() => {
    if (wasAnalyzing.current && !isAnalyzing) {
      load()
    }
    wasAnalyzing.current = isAnalyzing
  }, [isAnalyzing, load])

  const openFlow = useCallback(async (id: string) => {
    const gen = beginDocLoad()
    try {
      const full = await libraryApi.getContent(id)
      if (!isDocLoadCurrent(gen)) return
      setDocument(full as FlowDocument)
      useFlowStore.setState({libraryFlowId: id, libraryVersion: 0})
      setMainPaneView('block')
    } catch (e) {
      if (!isDocLoadCurrent(gen)) return
      toastRef.current.error('Failed to open flow', {
        description: e instanceof Error ? e.message : 'Unknown error',
      })
    }
  }, [setDocument, setMainPaneView])

  if (loading) {
    return (
      <div className="w-full h-full p-6 overflow-y-auto bg-surface-1">
        <SkeletonDashboard />
      </div>
    )
  }

  if (error || !data) {
    return (
      <div className="w-full h-full p-6 overflow-y-auto bg-surface-1">
        <div className="max-w-md mx-auto mt-20 flex flex-col items-center text-center gap-3">
          <div className="w-14 h-14 rounded-full bg-red-500/10 flex items-center justify-center">
            <AlertTriangle size={26} className="text-red-400" />
          </div>
          <div className="text-lg font-medium text-text-primary">Couldn't load your dashboard</div>
          {error && <div className="text-sm text-text-tertiary break-words">{error}</div>}
          <button
            onClick={retry}
            className="mt-1 inline-flex items-center gap-2 px-3 py-1.5 text-sm rounded-lg bg-surface-2 border border-border-subtle text-text-primary hover:bg-surface-3 transition-colors"
          >
            <RefreshCw size={14} /> Retry
          </button>
        </div>
      </div>
    )
  }

  const {greeting, overview, tokenUsage, recentFlows, findings, isCloud, healthTrend, costByProvider, ruleFrequency, activity, complexity, security, severityTrend, confidenceDist, healthBuckets, fixability, workflow} = data
  const orgName = greeting.activeOrgName || activeOrg?.name

  return (
    <div className="w-full h-full p-6 overflow-y-auto bg-surface-1">
      <div className="grid grid-cols-12 gap-4 max-w-7xl mx-auto">
        {/* Header */}
        <div className="col-span-12 mb-1">
          <div className="flex items-center gap-3">
            <h1 className="text-2xl font-bold text-text-primary">
              {timeGreeting()}, {greeting.userDisplayName}
            </h1>
            {isAnalyzing && (
              <span className="inline-flex items-center gap-1.5 text-xs text-brand-400 bg-brand-500/10 px-2 py-0.5 rounded-full animate-pulse">
                <span className="w-1.5 h-1.5 rounded-full bg-brand-400" />
                Analyzing…
              </span>
            )}
          </div>
          <p className="text-sm text-text-tertiary mt-0.5">
            {orgName ? `${orgName} · ` : ''}Here's your workspace at a glance.
            {!isCloud && ' Aggregates every flow analyzed this session — newest analysis per file.'}
          </p>
        </div>

        {/* Row 1: KPI Strip */}
        <KPIStripCard
          overview={overview}
          findings={findings}
          costByProvider={isCloud ? costByProvider : []}
          className="col-span-12"
        />

        {isCloud ? (
          <>
            {/* Row 2: Health Trend + Cost Breakdown */}
            <HealthTrendCard data={healthTrend} className="col-span-12 lg:col-span-8" />
            <CostBreakdownCard data={costByProvider} className="col-span-12 lg:col-span-4" />

            {/* Row 3: AI Token Usage + Health Gauge */}
            <AITokenUsageCard data={tokenUsage} className="col-span-12 lg:col-span-8" />
            <HealthGaugeCard overview={overview} bySeverity={findings.bySeverity} className="col-span-12 lg:col-span-4" />

            {/* Row 4: Rule Frequency + Findings Radar */}
            <RuleFrequencyCard data={ruleFrequency} className="col-span-12 lg:col-span-8" />
            <FindingsChartCard
              findings={findings}
              onOpenAnalytics={() => setMainPaneView('dashboard')}
              className="col-span-12 lg:col-span-4"
            />

            {/* Row 5: Severity Trend + Confidence Donut (developer analytics) */}
            <SeverityTrendCard data={severityTrend} className="col-span-12 lg:col-span-8" />
            <ConfidenceDonutCard confidence={confidenceDist} className="col-span-12 lg:col-span-4" />

            {/* Row 6: Recent Flows + Activity Feed */}
            <RecentFlowsCard flows={recentFlows} onOpen={openFlow} className="col-span-12 lg:col-span-7" />
            <ActivityFeedCard data={activity} className="col-span-12 lg:col-span-5" />

            {/* Row 7: Flow Complexity + Security Posture */}
            <FlowComplexityCard data={complexity} className="col-span-12 lg:col-span-7" />
            <SecurityPostureCard data={security} className="col-span-12 lg:col-span-5" />

            {/* Row 8: Health Distribution + Fix Availability (developer analytics) */}
            <HealthDistributionCard data={healthBuckets} className="col-span-12 lg:col-span-6" />
            <FixabilityCard data={fixability} className="col-span-12 lg:col-span-6" />

            {/* Row 9: Triage Workflow (cloud-only team health) */}
            <WorkflowFunnelCard data={workflow} className="col-span-12 lg:col-span-12" />
          </>
        ) : (
          <>
            {/* Row 2: Rule Frequency + Health Gauge */}
            <RuleFrequencyCard data={ruleFrequency} className="col-span-12 lg:col-span-8" />
            <HealthGaugeCard overview={overview} bySeverity={findings.bySeverity} className="col-span-12 lg:col-span-4" />

            {/* Row 3: Confidence + Health Distribution + Fix Availability (developer analytics) */}
            <ConfidenceDonutCard confidence={confidenceDist} className="col-span-12 lg:col-span-4" />
            <HealthDistributionCard data={healthBuckets} className="col-span-12 lg:col-span-4" />
            <FixabilityCard data={fixability} className="col-span-12 lg:col-span-4" />

            {/* Row 4: Recent Flows + Findings Radar */}
            <RecentFlowsCard flows={recentFlows} onOpen={openFlow} className="col-span-12 lg:col-span-7" />
            <FindingsChartCard
              findings={findings}
              onOpenAnalytics={() => setMainPaneView('dashboard')}
              className="col-span-12 lg:col-span-5"
            />

            {/* Row 5: Flow Complexity */}
            <FlowComplexityCard data={complexity} className="col-span-12" />
          </>
        )}
      </div>
    </div>
  )
}

function timeGreeting(): string {
  const h = new Date().getHours()
  if (h < 12) return 'Good morning'
  if (h < 18) return 'Good afternoon'
  return 'Good evening'
}
