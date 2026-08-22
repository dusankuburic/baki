import React from 'react'
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
            <p className="text-sm font-medium text-text-primary">Chat panel error</p>
            <p className="text-xs text-text-tertiary mt-1">Something went wrong rendering this thread.</p>
          </div>
          <button
            onClick={() => this.setState({error: null})}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-surface-3 border border-border-default text-xs text-text-secondary hover:text-text-primary hover:bg-surface-4 transition-colors"
          >
            <RefreshCw size={12} />
            Try again
          </button>
        </div>
      )
    }
    return this.props.children
  }
}
