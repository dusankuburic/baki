import {Component, type ReactNode} from 'react'
import {systemApi} from '@/api'
// Class component: no hooks, so translate through the i18next instance
// directly. i18n init is synchronous at import, so t() is always ready. The
// trade-off is that this fallback does not re-render on a language change —
// acceptable for an error screen the user is about to dismiss.
import i18n from '@/i18n'
import {isChunkLoadError, reloadForChunkError, reloadPage} from '@/lib/chunkLoad'
import {logger} from '@/lib/logger'

type ErrorBoundaryProps = {
  children: ReactNode
  fallback?: ReactNode
  fallbackMessage?: string
}

type ErrorBoundaryState = {
  hasError: boolean
  error: Error | null
}

export default class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = {hasError: false, error: null}

  static getDerivedStateFromError(error: Error) {
    return {hasError: true, error}
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    // A lazy chunk that failed to download (a network drop, or a redeploy that
    // removed the hashed file this tab still points at) only recovers with a
    // reload: retrying in place re-throws the failure that the browser and
    // React.lazy both keep (see lib/chunkLoad). One automatic reload per
    // window, so a chunk broken in the deployed build cannot reload-loop the
    // page; after that the fallback offers the reload as a button.
    if (isChunkLoadError(error) && reloadForChunkError()) return
    logger.error('ErrorBoundary caught:', error, info)
    try {
      // .catch() on the promise itself, not just this try/catch: the try only
      // guards a SYNCHRONOUS throw, so an unreachable backend left an unhandled
      // rejection. It happened to be absorbed by useGlobalErrorHandler's
      // unhandledrejection listener (whose own report is guarded), but relying
      // on that is accidental — handle it here. Same rationale as the explicit
      // swallow documented in useGlobalErrorHandler.ts.
      void systemApi
        .logError({
          message: error.message,
          stack: error.stack || '',
          componentStack: info.componentStack || '',
          // Send the pathname only — never the full href. Recovery/SSO
          // tokens (#resetPassword=…, #verifyEmail=…, #invite=…, the SSO
          // ticket) live in the fragment, and a render error during a
          // deep-link load could otherwise capture them into the error
          // report before the fragment is cleared.
          url: window.location.pathname,
        })
        .catch(() => {
          // backend unreachable — a failed error report must not itself raise
        })
    } catch {
      // backend not available during SSR or tests
    }
  }

  private retry = () => this.setState({hasError: false, error: null})

  render() {
    if (this.state.hasError) {
      if (this.props.fallback) return this.props.fallback
      // "Try again" re-renders the same rejected lazy import, so a failed
      // chunk gets the reload that actually fixes it.
      const chunkFailed = isChunkLoadError(this.state.error)
      return (
        <div className="flex flex-col items-center justify-center p-8">
          <div className="text-semantic-error text-sm font-medium mb-2">
            {chunkFailed ? i18n.t('common:chunkError.title') : (this.props.fallbackMessage ?? i18n.t('common:error'))}
          </div>
          <div className="text-xs text-text-tertiary max-w-md text-center">
            {chunkFailed ? i18n.t('common:chunkError.body') : this.state.error?.message}
          </div>
          <button
            onClick={chunkFailed ? reloadPage : this.retry}
            className="mt-4 text-xs text-brand-400 hover:text-brand-300"
          >
            {chunkFailed ? i18n.t('common:chunkError.reload') : i18n.t('common:tryAgain')}
          </button>
        </div>
      )
    }
    return this.props.children
  }
}
