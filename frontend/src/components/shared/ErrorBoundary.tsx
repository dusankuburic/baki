import {Component, type ReactNode} from 'react'
import {systemApi} from '@/api'

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
        console.error('ErrorBoundary caught:', error, info)
        try {
            systemApi.logError({
                message: error.message,
                stack: error.stack || '',
                componentStack: info.componentStack || '',
                url: window.location.href,
            })
        } catch {
            // backend not available during SSR or tests
        }
    }

    private retry = () => this.setState({hasError: false, error: null})

    render() {
        if (this.state.hasError) {
            if (this.props.fallback) return this.props.fallback
            return (
                <div className="flex flex-col items-center justify-center p-8">
                    <div className="text-semantic-error text-sm font-medium mb-2">
                        {this.props.fallbackMessage ?? 'Something went wrong'}
                    </div>
                    <div className="text-xs text-text-tertiary max-w-md text-center">
                        {this.state.error?.message}
                    </div>
                    <button
                        onClick={this.retry}
                        className="mt-4 text-xs text-brand-400 hover:text-brand-300"
                    >
                        Try again
                    </button>
                </div>
            )
        }
        return this.props.children
    }
}
