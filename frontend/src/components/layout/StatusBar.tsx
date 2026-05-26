import {Check, AlertTriangle, Loader} from 'lucide-react'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'

export default function StatusBar() {
    const document = useFlowStore(s => s.document)
    const parseErrors = document?.parseErrors
    const isParsing = useFlowStore(s => s.isParsing)
    const parseProgress = useFlowStore(s => s.parseProgress)
    const isAnalyzing = useAnalysisStore(s => s.isAnalyzing)
    const progress = useAnalysisStore(s => s.progress)
    const report = useAnalysisStore(s => document ? s.reports.get(document.id) : undefined)

    const hasErrors = parseErrors && parseErrors.length > 0
    const blockCount = document?.metadata?.blockCount ?? 0
    const subflowCount = document?.metadata?.subflowCount ?? 0

    const findingTotal = report ? report.stats.errors + report.stats.warnings + report.stats.info : 0

    return (
        <div className="flex items-center h-6 px-3 border-t border-border-subtle bg-surface-1 text-xs text-text-tertiary">
            <div className="flex items-center gap-1.5 flex-1">
                {document ? (
                    <>
                        {hasErrors ? (
                            <AlertTriangle size={12} className="text-semantic-warning" />
                        ) : (
                            <Check size={12} className="text-semantic-success" />
                        )}
                        <span>
                            Parsed {blockCount} blocks · {subflowCount} subflows
                            {hasErrors && ` · ${parseErrors.length} warnings`}
                        </span>
                        {isAnalyzing && (
                            <span className="flex items-center gap-1 ml-2">
                                <Loader size={10} className="animate-spin" />
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
                        <Loader size={10} className="animate-spin" />
                        Parsing... {parseProgress > 0 ? `${parseProgress}%` : ''}
                    </span>
                ) : (
                    <span>Ready</span>
                )}
            </div>
            <div className="flex items-center gap-2">
                {document && (
                    <span>{document.name}</span>
                )}
            </div>
        </div>
    )
}
