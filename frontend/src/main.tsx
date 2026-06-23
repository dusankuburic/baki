import React from 'react'
import {createRoot} from 'react-dom/client'
import './index.css'
import App from './App'
import ProtectedRoute from './components/auth/ProtectedRoute'

const container = document.getElementById('root')
if (!container) throw new Error('Root element #root not found')
const root = createRoot(container)

root.render(
    <React.StrictMode>
        <ProtectedRoute>
            <App/>
        </ProtectedRoute>
    </React.StrictMode>
)
