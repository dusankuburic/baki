import {useEffect, useState} from 'react'
import {Check, User, Shield} from 'lucide-react'
import {Spinner} from '@/components/shared'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useAuthStore} from '@/stores/authStore'
import {useUIStore} from '@/stores/uiStore'
import {subscribeConnectionState, type EventConnectionState} from '@/api/client'

export default function StatusBar() {
    const document = useFlowStore(s => s.document)
    const isParsing = useFlowStore(s => s.isParsing)
    const parseProgress = useFlowStore(s => s.parseProgress)
    const isAnalyzing = useAnalysisStore(s => s.isAnalyzing)
    const progress = useAnalysisStore(s => s.progress)
    const report = useAnalysisStore(s => document ? s.reports.get(document.id) : undefined)
    const user = useAuthStore(s => s.user)
    const setMainPaneView = useUIStore(s => s.setMainPaneView)

    const [conn, setConn] = useState<EventConnectionState>('idle')
    useEffect(() => subscribeConnectionState(setConn), [])

    const blockCount = document?.metadata?.blockCount ?? 0
    const subflowCount = document?.metadata?.subflowCount ?? 0

    const findingTotal = report ? report.stats.errors + report.stats.warnings + report.stats.info : 0

    return (
        <div className="flex items-center h-6 px-3 border-t border-border-subtle bg-surface-1 text-xs text-text-tertiary">
            <div className="flex items-center gap-1.5 flex-1">
                {document ? (
                    <>
                        <Check size={12} className="text-semantic-success" />
                        <span>
                            Parsed {blockCount} blocks · {subflowCount} subflows
                        </span>
                        {isAnalyzing && (
                            <span className="flex items-center gap-1 ml-2">
                                <Spinner size={10} />
                                Analyzing... {progress.total > 0 ? Math.round(progress.current / progress.total * 100) : 0}%
                            </span>
                        )}
                        {!isAnalyzing && report && (
                            <span className="ml-2">
                                · {findingTotal} findings ({report.stats.errors} errors, {report.stats.warnings} warnings)
                            </span>
                        )}
                    </>
                ) : isParsing ? (
                    <span className="flex items-center gap-1">
                        <Spinner size={10} />
                        Parsing... {parseProgress > 0 ? `${parseProgress}%` : ''}
                    </span>
                ) : (
                    <span>Ready</span>
                )}
            </div>
            <div className="flex items-center gap-4">
                {(conn === 'reconnecting' || conn === 'connecting') && (
                    <span
                        className="flex items-center gap-1 text-semantic-warning"
                        title="Live updates connection"
                    >
                        <Spinner size={10} />
                        {conn === 'reconnecting' ? 'Reconnecting…' : 'Connecting…'}
                    </span>
                )}
                {user && (
                    <>
                        {user.role === 'admin' && (
                            <button
                                onClick={() => setMainPaneView('admin')}
                                className="flex items-center gap-1 hover:text-text-secondary transition-colors"
                                title="Admin Dashboard"
                            >
                                <Shield size={12} />
                                <span>Admin</span>
                            </button>
                        )}
                        <button
                            onClick={() => setMainPaneView('profile')}
                            className="flex items-center gap-1 hover:text-text-secondary transition-colors"
                            title="User Profile"
                        >
                            <User size={12} />
                            <span>{user.email}</span>
                        </button>
                    </>
                )}
                {document && (
                    <span>{document.name}</span>
                )}
            </div>
        </div>
    )
}
