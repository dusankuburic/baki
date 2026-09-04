import React from 'react'
// Class component: no hooks, so translate through the i18next instance (see
// the same note in shared/ErrorBoundary).
import i18n from '@/i18n'
import {AlertTriangle, RefreshCw} from 'lucide-react'
import {logger} from '@/lib/logger'

interface State {
  error: Error | null
}

export default class ChatErrorBoundary extends React.Component<{children: React.ReactNode}, State> {
  state: State = {error: null}

  static getDerivedStateFromError(error: Error): State {
    return {error}
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    logger.error('ChatErrorBoundary', {message: error.message, componentStack: info.componentStack})
  }

  render() {
    if (this.state.error) {
      return (
        <div className="flex flex-col items-center justify-center h-full gap-3 p-6 text-center">
          <AlertTriangle size={20} className="text-semantic-warning" />
          <div>
            <p className="text-sm font-medium text-text-primary">{i18n.t('chat:errorBoundary.title')}</p>
            <p className="text-xs text-text-tertiary mt-1">{i18n.t('chat:errorBoundary.body')}</p>
          </div>
          <button
            onClick={() => this.setState({error: null})}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-surface-3 border border-border-default text-xs text-text-secondary hover:text-text-primary hover:bg-surface-4 transition-colors"
          >
            <RefreshCw size={12} />
            {i18n.t('chat:errorBoundary.retry')}
          </button>
        </div>
      )
    }
    return this.props.children
  }
}
