import React from 'react'
import {createRoot} from 'react-dom/client'
import './i18n'
import './index.css'
import App from './App'
import ProtectedRoute from './components/auth/ProtectedRoute'
import SharedReportView from './components/shared/SharedReportView'
import ErrorBoundary from './components/shared/ErrorBoundary'
import {isTauri} from './platform/guards'
import {isSystemView, useUIStore, type MainPaneView} from './stores/uiStore'

const container = document.getElementById('root')
if (!container) throw new Error('Root element #root not found')
const root = createRoot(container)

const isSharedView = window.location.pathname.endsWith('/shared')
if (!isTauri() && !isSharedView) {
  const view = new URLSearchParams(window.location.search).get('view')
  if (view && isSystemView(view as MainPaneView)) {
    useUIStore.getState().setMainPaneView(view as MainPaneView)
  }
}

const swallow = () => {}
if (!isSharedView) {
  if (useUIStore.getState().mainPaneView === 'home') void import('@/components/dashboard/HomeDashboard').catch(swallow)
  void import('@/components/flow/BlockView').catch(swallow)
}

if (import.meta.env.PROD && !isTauri() && 'serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch(() => {
    })
  })
}

if (isSharedView) {
  root.render(
    <React.StrictMode>
      <ErrorBoundary>
        <SharedReportView />
      </ErrorBoundary>
    </React.StrictMode>,
  )
} else {
  root.render(
    <React.StrictMode>
      <ErrorBoundary>
        <ProtectedRoute>
          <App />
        </ProtectedRoute>
      </ErrorBoundary>
    </React.StrictMode>,
  )
}
