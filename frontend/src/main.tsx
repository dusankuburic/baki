import React from 'react'
import {createRoot} from 'react-dom/client'
import './index.css'
import App from './App'
import ProtectedRoute from './components/auth/ProtectedRoute'

const container = document.getElementById('root')
const root = createRoot(container!)

root.render(
    <React.StrictMode>
        <ProtectedRoute>
            <App/>
        </ProtectedRoute>
    </React.StrictMode>
)
