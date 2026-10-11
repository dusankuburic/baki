import {lazy, Suspense, type ReactNode} from 'react'
import {Spinner, ErrorBoundary} from '@/components/shared'
import {UserProfile} from '@/components/auth/UserProfile'
import {MainPaneToolbar} from '@/components/flow'
import {useSystemStore} from '@/stores/systemStore'

function ViewBoundary({children}: {children: ReactNode}) {
  return (
    <ErrorBoundary>
      <Suspense fallback={<Spinner />}>{children}</Suspense>
    </ErrorBoundary>
  )
}

const AnalyticsDashboard = lazy(() => import('@/components/dashboard/AnalyticsDashboard'))
const CloudAnalyticsPage = lazy(() => import('@/components/dashboard/analytics/CloudAnalyticsPage'))
const HomeDashboard = lazy(() => import('@/components/dashboard/HomeDashboard'))
const LibraryWorkspace = lazy(() => import('@/components/library/LibraryWorkspace'))
const PortfolioView = lazy(() => import('@/components/dashboard/PortfolioView'))
const FlowComparisonView = lazy(() => import('@/components/flow/FlowComparisonView'))
const RuleDependencyView = lazy(() => import('@/components/analyzer/RuleDependencyView'))
const RuleReference = lazy(() => import('@/components/analyzer/RuleReference'))
const AdminDashboard = lazy(() => import('@/components/admin/AdminDashboard').then(m => ({default: m.AdminDashboard})))

export function SystemViewRouter({view}: {view: string}) {
  const sessionAnalytics = useSystemStore(s => s.info?.capabilities.sessionAnalytics ?? false)
  const capabilitiesLoaded = useSystemStore(s => s.isLoaded)

  switch (view) {
    case 'profile':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-y-auto">
            <UserProfile />
          </div>
        </div>
      )
    case 'admin':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-hidden">
            <ViewBoundary>
              <AdminDashboard />
            </ViewBoundary>
          </div>
        </div>
      )
    case 'dashboard':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          {!capabilitiesLoaded ? (
            <div className="flex-1 flex items-center justify-center">
              <Spinner />
            </div>
          ) : sessionAnalytics ? (
            <div className="flex-1 overflow-y-auto p-4">
              <ViewBoundary>
                <AnalyticsDashboard />
              </ViewBoundary>
            </div>
          ) : (
            <div className="flex-1 overflow-hidden">
              <ViewBoundary>
                <CloudAnalyticsPage />
              </ViewBoundary>
            </div>
          )}
        </div>
      )
    case 'home':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-hidden">
            <ViewBoundary>
              <HomeDashboard />
            </ViewBoundary>
          </div>
        </div>
      )
    case 'library':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-hidden">
            <ViewBoundary>
              <LibraryWorkspace />
            </ViewBoundary>
          </div>
        </div>
      )
    case 'portfolio':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-hidden">
            <ViewBoundary>
              <PortfolioView />
            </ViewBoundary>
          </div>
        </div>
      )
    case 'flow-compare':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-auto">
            <ViewBoundary>
              <FlowComparisonView />
            </ViewBoundary>
          </div>
        </div>
      )
    case 'deps':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-hidden">
            <ViewBoundary>
              <RuleDependencyView />
            </ViewBoundary>
          </div>
        </div>
      )
    case 'rules':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-y-auto">
            <ViewBoundary>
              <RuleReference />
            </ViewBoundary>
          </div>
        </div>
      )
    default:
      return null
  }
}

export {isSystemView} from '@/stores/uiStore'
