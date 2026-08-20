import React from 'react'
import {createRoot} from 'react-dom/client'
import './i18n'
import './index.css'
import App from './App'
import ProtectedRoute from './components/auth/ProtectedRoute'
import SharedReportView from './components/shared/SharedReportView'
import {isTauri} from './platform/guards'
import {isSystemView, useUIStore, type MainPaneView} from './stores/uiStore'

const container = document.getElementById('root')
if (!container) throw new Error('Root element #root not found')
const root = createRoot(container)

// Honor a ?view=<system-view> deep link on boot (PWA shortcuts, bookmarks,
// external links like /?view=library). Unknown values are ignored.
const isSharedView = window.location.pathname.endsWith('/shared')
if (!isTauri() && !isSharedView) {
  const view = new URLSearchParams(window.location.search).get('view')
  if (view && isSystemView(view as MainPaneView)) {
    useUIStore.getState().setMainPaneView(view as MainPaneView)
  }
}

// Register the service worker for offline shell caching — web mode only. In
// Tauri the frontend is served from a localhost sidecar and a SW would
// interfere with the IPC bridge.
if (!isTauri() && 'serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch(() => {
      // SW registration failure is non-fatal — the app works online regardless.
    })
  })
}

// The /shared path is an UNAUTHENTICATED public viewer for share-link
// recipients — it renders outside the app shell so no auth/stores are needed.
if (isSharedView) {
  root.render(
    <React.StrictMode>
      <SharedReportView />
    </React.StrictMode>,
  )
} else {
  root.render(
    <React.StrictMode>
      <ProtectedRoute>
        <App />
      </ProtectedRoute>
    </React.StrictMode>,
  )
}
