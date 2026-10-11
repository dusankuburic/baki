import {Component, type ReactNode} from 'react'
import {systemApi} from '@/api'
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
    if (isChunkLoadError(error) && reloadForChunkError()) return
    logger.error('ErrorBoundary caught:', error, info)
    try {
      void systemApi
        .logError({
          message: error.message,
          stack: error.stack || '',
          componentStack: info.componentStack || '',
          url: window.location.pathname,
        })
        .catch(() => {
        })
    } catch {
    }
  }

  private retry = () => this.setState({hasError: false, error: null})

  render() {
    if (this.state.hasError) {
      if (this.props.fallback) return this.props.fallback
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
