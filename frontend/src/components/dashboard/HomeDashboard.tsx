import {useCallback, useEffect, useState} from 'react'
import {RefreshCw, AlertTriangle, Layers, GitBranch} from 'lucide-react'
import {dashboardApi, libraryApi} from '@/api'
import {logger} from '@/lib/logger'
import {useToast} from '@/components/shared'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import type {DashboardHomeData, FlowDocument} from '@/types/domain'
import {HealthGaugeCard} from './home/HealthGaugeCard'
import {AITokenUsageCard} from './home/AITokenUsageCard'
import {FindingsChartCard} from './home/FindingsChartCard'
import {RecentFlowsCard} from './home/RecentFlowsCard'
import {SkeletonDashboard} from './home/SkeletonDashboard'

export default function HomeDashboard() {
  const [data, setData] = useState<DashboardHomeData | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const setDocument = useFlowStore(s => s.setDocument)
  const toast = useToast()

  // Promise-callback form (not async/await): setState runs inside .then/.catch,
  // which keeps it out of the effect's synchronous body. The loading reset for
  // retry lives in the button handler, not an effect.
  const load = useCallback(() => {
    dashboardApi.getHome()
      .then(d => { setData(d); setError(null) })
      .catch(e => {
        logger.error('dashboard: load failed', e)
        setError(e instanceof Error ? e.message : 'Failed to load dashboard')
      })
      .finally(() => setLoading(false))
  }, [])

  const retry = useCallback(() => {
    setLoading(true)
    setError(null)
    load()
  }, [load])

  useEffect(() => {
    load()
  }, [load])

  const openFlow = useCallback(async (id: string) => {
    try {
      const full = await libraryApi.getContent(id)
      setDocument(full as FlowDocument)
      useFlowStore.setState({libraryFlowId: id, libraryVersion: 0})
      setMainPaneView('block')
    } catch (e) {
      toast.error('Failed to open flow', {
        description: e instanceof Error ? e.message : 'Unknown error',
      })
    }
  }, [setDocument, setMainPaneView, toast])

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
          <div className="text-lg font-medium text-text-primary">Couldn’t load your dashboard</div>
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

  const {greeting, overview, tokenUsage, recentFlows, findings} = data

  return (
    <div className="w-full h-full p-6 overflow-y-auto bg-surface-1">
      <div className="grid grid-cols-12 gap-4 max-w-7xl mx-auto">
        {/* Header */}
        <div className="col-span-12 flex items-center justify-between mb-2">
          <div>
            <h1 className="text-2xl font-bold text-text-primary">
              {timeGreeting()}, {greeting.userDisplayName}
            </h1>
            <p className="text-sm text-text-tertiary mt-0.5">
              {greeting.activeOrgName ? `${greeting.activeOrgName} · ` : ''}Here’s your workspace at a glance.
            </p>
          </div>
          <div className="hidden sm:flex items-center gap-4 text-sm">
            <Stat icon={<Layers size={15} />} label="Flows" value={overview.totalFlows} />
            <Stat icon={<GitBranch size={15} />} label="Subflows" value={overview.totalSubflows} />
          </div>
        </div>

        {/* Row 2 */}
        <HealthGaugeCard overview={overview} bySeverity={findings.bySeverity} className="col-span-12 lg:col-span-4" />
        <AITokenUsageCard data={tokenUsage} className="col-span-12 lg:col-span-8" />

        {/* Row 3 */}
        <RecentFlowsCard flows={recentFlows} onOpen={openFlow} className="col-span-12 lg:col-span-7" />
        <FindingsChartCard
          findings={findings}
          onOpenAnalytics={() => setMainPaneView('dashboard')}
          className="col-span-12 lg:col-span-5"
        />
      </div>
    </div>
  )
}

function Stat({icon, label, value}: {icon: React.ReactNode; label: string; value: number}) {
  return (
    <div className="flex items-center gap-1.5 text-text-tertiary">
      {icon}
      <span className="font-mono font-semibold tabular-nums text-text-primary">{value}</span>
      <span className="text-text-tertiary">{label}</span>
    </div>
  )
}

function timeGreeting(): string {
  const h = new Date().getHours()
  if (h < 12) return 'Good morning'
  if (h < 18) return 'Good afternoon'
  return 'Good evening'
}
