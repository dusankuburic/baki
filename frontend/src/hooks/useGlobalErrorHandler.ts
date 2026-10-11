import {useEffect} from 'react'
import {systemApi} from '@/api'

function report(message: string, stack: string, componentStack: string, url: string) {
  try {
    const result = systemApi.logError({message, stack, componentStack, url})
    if (result && typeof result.catch === 'function') {
      result.catch(() => {
      })
    }
  } catch {
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
