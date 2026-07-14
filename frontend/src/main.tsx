import React from 'react'
import {createRoot} from 'react-dom/client'
import './index.css'
import App from './App'
import ProtectedRoute from './components/auth/ProtectedRoute'
import SharedReportView from './components/shared/SharedReportView'
import {isTauri} from './platform/guards'

const container = document.getElementById('root')
if (!container) throw new Error('Root element #root not found')
const root = createRoot(container)

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
if (window.location.pathname.endsWith('/shared')) {
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
