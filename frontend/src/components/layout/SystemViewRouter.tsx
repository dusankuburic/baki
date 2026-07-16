import {lazy, Suspense} from 'react'
import {Spinner} from '@/components/shared'
import {UserProfile} from '@/components/auth/UserProfile'
import {MainPaneToolbar} from '@/components/flow'

// GraphView pulls in cytoscape (~100KB+); keep it out of the entry chunk like
// its lazy siblings below.
// HomeDashboard pulls in recharts (~100KB+); lazy-load keeps it out of the entry chunk.
// LibraryWorkspace pulls in the multi-pane browse experience and its helpers;
// lazy keeps it out of the entry chunk and only loads when the user opens it.
// AdminDashboard is admin-only; lazy keeps it out of every non-admin's bundle.
const AnalyticsDashboard = lazy(() => import('@/components/dashboard/AnalyticsDashboard'))
const HomeDashboard = lazy(() => import('@/components/dashboard/HomeDashboard'))
const LibraryWorkspace = lazy(() => import('@/components/library/LibraryWorkspace'))
const PortfolioView = lazy(() => import('@/components/dashboard/PortfolioView'))
const RuleDependencyView = lazy(() => import('@/components/analyzer/RuleDependencyView'))
const RuleReference = lazy(() => import('@/components/analyzer/RuleReference'))
const AdminDashboard = lazy(() => import('@/components/admin/AdminDashboard').then(m => ({default: m.AdminDashboard})))

// SystemViewRouter renders the top-level views that are NOT the flow editor:
// profile, admin, analytics/home dashboards, the library browser, and the
// portfolio. Extracted from MainPane so MainPane is a thin router and these
// full-pane views are isolated (each owns its own toolbar + scroll container).
//
// Returns null for flow-editor views ('block', 'graph', 'map', 'local-map',
// 'diff', '' …) so the caller knows to render the editor groups instead.
export function SystemViewRouter({view}: {view: string}) {
  switch (view) {
    case 'profile':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-y-auto p-4">
            <UserProfile />
          </div>
        </div>
      )
    case 'admin':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-y-auto p-4">
            <Suspense fallback={<Spinner />}>
              <AdminDashboard />
            </Suspense>
          </div>
        </div>
      )
    case 'dashboard':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-y-auto p-4">
            <Suspense fallback={<Spinner />}>
              <AnalyticsDashboard />
            </Suspense>
          </div>
        </div>
      )
    case 'home':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-hidden">
            <Suspense fallback={<Spinner />}>
              <HomeDashboard />
            </Suspense>
          </div>
        </div>
      )
    case 'library':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <Suspense fallback={<Spinner />}>
            <LibraryWorkspace />
          </Suspense>
        </div>
      )
    case 'portfolio':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-hidden">
            <Suspense fallback={<Spinner />}>
              <PortfolioView />
            </Suspense>
          </div>
        </div>
      )
    case 'deps':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-hidden">
            <Suspense fallback={<Spinner />}>
              <RuleDependencyView />
            </Suspense>
          </div>
        </div>
      )
    case 'rules':
      return (
        <div className="flex flex-col h-full bg-surface-1">
          <MainPaneToolbar />
          <div className="flex-1 overflow-y-auto">
            <Suspense fallback={<Spinner />}>
              <RuleReference />
            </Suspense>
          </div>
        </div>
      )
    default:
      return null
  }
}

// isSystemView reports whether `view` is handled by SystemViewRouter (i.e. not
// a flow-editor view). MainPane uses this to decide between SystemViewRouter
// and the FlowEditorPane.
export function isSystemView(view: string): boolean {
  return (
    view === 'profile' ||
    view === 'admin' ||
    view === 'dashboard' ||
    view === 'home' ||
    view === 'library' ||
    view === 'portfolio' ||
    view === 'deps' ||
    view === 'rules'
  )
}
