import React from 'react'
import {createRoot} from 'react-dom/client'
import './index.css'
import App from './App'
import ProtectedRoute from './components/auth/ProtectedRoute'
import SharedReportView from './components/shared/SharedReportView'

const container = document.getElementById('root')
if (!container) throw new Error('Root element #root not found')
const root = createRoot(container)

// The /shared path is an UNAUTHENTICATED public viewer for share-link
// recipients — it renders outside the app shell so no auth/stores are needed.
if (window.location.pathname === '/shared') {
    root.render(
        <React.StrictMode>
            <SharedReportView/>
        </React.StrictMode>
    )
} else {
    root.render(
        <React.StrictMode>
            <ProtectedRoute>
                <App/>
            </ProtectedRoute>
        </React.StrictMode>
    )
}
