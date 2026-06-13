import {useEffect} from 'react'
import {systemApi} from '@/api'

export function useGlobalErrorHandler() {
    useEffect(() => {
        const handler = (event: ErrorEvent) => {
            systemApi.logError({message: event.message, stack: event.error?.stack || '', componentStack: '', url: event.filename})
        }
        const rejectionHandler = (event: PromiseRejectionEvent) => {
            systemApi.logError({message: String(event.reason), stack: '', componentStack: '', url: ''})
        }
        window.addEventListener('error', handler)
        window.addEventListener('unhandledrejection', rejectionHandler)
        return () => {
            window.removeEventListener('error', handler)
            window.removeEventListener('unhandledrejection', rejectionHandler)
        }
    }, [])
}
