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

// Warm the chunks the first screen needs, in parallel with the entry's own
// evaluation. There are no modulepreload hints (vite.config.ts explains why),
// so without this the first paint is a chain of serial round trips: entry →
// React mounts → the router's lazy(HomeDashboard) → its own imports. Vite
// dedupes the dynamic-import promise, so the later `lazy()` picks up the
// request already in flight. The Home chunk only when Home is what the shell
// opens on (the default, unless a ?view= deep link chose another system
// view); BlockView is where opening any flow lands, so always.
// A chunk that fails here fails again at the lazy() that needs it, where the
// error boundary handles it; the warm-up itself reports nothing.
const swallow = () => {}
if (!isSharedView) {
  if (useUIStore.getState().mainPaneView === 'home') void import('@/components/dashboard/HomeDashboard').catch(swallow)
  void import('@/components/flow/BlockView').catch(swallow)
}

// Register the service worker for offline shell caching — web + production
// builds only. In Tauri the frontend is served from a localhost sidecar and a
// SW would interfere with the IPC bridge; under `vite dev` the SW's
// stale-while-revalidate caching of same-origin modules would serve outdated
// code during HMR.
if (import.meta.env.PROD && !isTauri() && 'serviceWorker' in navigator) {
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
