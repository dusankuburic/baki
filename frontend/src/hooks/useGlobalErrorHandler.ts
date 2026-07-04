import {useEffect} from 'react'
import {systemApi} from '@/api'

// Reports a client-side error to the backend. The returned Promise MUST be
// caught: this handler also receives `unhandledrejection` events, so an
// uncaught rejection here (e.g. when the backend is down) would fire a fresh
// `unhandledrejection` event → re-enter this handler → call logError again →
// reject again… a self-sustaining loop that hammers an already-struggling
// backend and never stops. Swallow the rejection to break the cycle.
function report(message: string, stack: string, componentStack: string, url: string) {
    try {
        const result = systemApi.logError({message, stack, componentStack, url})
        if (result && typeof result.catch === 'function') {
            result.catch(() => {
                /* backend unavailable — swallow to avoid a rejection loop */
            })
        }
    } catch {
        /* logError threw synchronously (e.g. before the fetch) — swallow */
    }
}

export function useGlobalErrorHandler() {
    useEffect(() => {
        const handler = (event: ErrorEvent) => {
            report(event.message, event.error?.stack || '', '', event.filename)
        }
        const rejectionHandler = (event: PromiseRejectionEvent) => {
            report(String(event.reason), '', '', '')
        }
        window.addEventListener('error', handler)
        window.addEventListener('unhandledrejection', rejectionHandler)
        return () => {
            window.removeEventListener('error', handler)
            window.removeEventListener('unhandledrejection', rejectionHandler)
        }
    }, [])
}
