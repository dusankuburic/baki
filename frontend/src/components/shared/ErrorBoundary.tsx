import {Component, type ReactNode} from 'react'
import {systemApi} from '@/api'
// Class component: no hooks, so translate through the i18next instance
// directly. i18n init is synchronous at import, so t() is always ready. The
// trade-off is that this fallback does not re-render on a language change —
// acceptable for an error screen the user is about to dismiss.
import i18n from '@/i18n'
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
    // A dynamic import that 404s means this tab predates the currently
    // deployed bundle — the hashed chunk filenames it references no longer
    // exist (e.g. a redeploy happened while the tab stayed open). Retrying
    // in-page can't help because the old index.html still points at the old
    // chunk names; the only fix is reloading the document. Guarded by
    // sessionStorage so a chunk that is genuinely broken in the NEW build
    // cannot reload-loop the page.
    if (this.isStaleChunkError(error)) {
      try {
        if (!sessionStorage.getItem('baki.stale-chunk-reloaded')) {
          sessionStorage.setItem('baki.stale-chunk-reloaded', '1')
          window.location.reload()
          return
        }
      } catch {
        // sessionStorage unavailable — fall through to normal error handling
      }
    }
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

  // Vite's lazy chunks fail with these messages when the file is gone from
  // the server (Chromium, Firefox, older Safari phrasings respectively).
  private isStaleChunkError(error: Error): boolean {
    return (
      /Failed to fetch dynamically imported module/i.test(error.message) ||
      /Importing a module script failed/i.test(error.message) ||
      /error loading dynamically imported module/i.test(error.message)
    )
  }

  render() {
    if (this.state.hasError) {
      if (this.props.fallback) return this.props.fallback
      return (
        <div className="flex flex-col items-center justify-center p-8">
          <div className="text-semantic-error text-sm font-medium mb-2">
            {this.props.fallbackMessage ?? i18n.t('common:error')}
          </div>
          <div className="text-xs text-text-tertiary max-w-md text-center">{this.state.error?.message}</div>
          <button onClick={this.retry} className="mt-4 text-xs text-brand-400 hover:text-brand-300">
            {i18n.t('common:tryAgain')}
          </button>
        </div>
      )
    }
    return this.props.children
  }
}
