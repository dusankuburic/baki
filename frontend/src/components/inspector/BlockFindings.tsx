import {useMemo} from 'react'
import {AlertTriangle, AlertCircle, Info, Sparkles} from 'lucide-react'
import clsx from 'clsx'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useChatStore} from '@/stores/chatStore'
import {useUIStore} from '@/stores/uiStore'
import {categoryBadgeClass} from '@/lib/findingsColors'
import type {Finding, Severity} from '@/types'

const severityIcon: Record<Severity, typeof AlertTriangle> = {
    error: AlertCircle,
    warning: AlertTriangle,
    info: Info,
}

const severityColor: Record<Severity, string> = {
    error: 'text-red-400',
    warning: 'text-amber-400',
    info: 'text-blue-400',
}

const severityRank: Record<Severity, number> = {error: 0, warning: 1, info: 2}

export default function BlockFindings() {
    const docId = useFlowStore(s => s.document?.id ?? null)
    const blockId = useFlowStore(s => s.selectedBlockId)
    const report = useAnalysisStore(s => docId ? s.reports.get(docId) : undefined)
    const setInspectorTab = useUIStore(s => s.setInspectorTab)
    const setInspectorCollapsed = useUIStore(s => s.setInspectorCollapsed)
    const appendMessage = useChatStore(s => s.appendMessage)
    const createThread = useChatStore(s => s.createThread)
    const updateThread = useChatStore(s => s.updateThread)
    const switchThread = useChatStore(s => s.switchThread)

    const blockFindings = useMemo(() => {
        if (!report || !blockId) return []
        return report.findings
            .filter(f => f.blockId === blockId)
            .sort((a, b) => severityRank[a.severity] - severityRank[b.severity])
    }, [report, blockId])

    if (blockFindings.length === 0) return null

    const handleFixWithAI = (finding: Finding) => {
        if (!docId) return
        const threadId = createThread(docId)
        updateThread(threadId, {
            title: `Fix: ${finding.title}`,
            contextBlockId: finding.blockId,
            useTools: true,
        })
        const parts = [
            `Help me fix this issue: **${finding.title}**`,
            finding.description,
            finding.suggestion ? `Suggestion: ${finding.suggestion}` : '',
            finding.autoFixHint ? `Analyzer fix hint:\n\`\`\`\n${finding.autoFixHint}\n\`\`\`` : '',
            `Rule: \`${finding.ruleId}\` · Severity: ${finding.severity}`,
        ].filter(Boolean)
        appendMessage(threadId, {
            id: crypto.randomUUID(),
            role: 'user',
            content: parts.join('\n\n'),
            timestamp: new Date().toISOString(),
            contextBlockId: finding.blockId,
        })
        switchThread(threadId)
        setInspectorTab('ai')
        setInspectorCollapsed(false)
    }

    return (
        <div className="rounded-lg border border-border-subtle overflow-hidden">
            <div className="px-3 py-2 bg-surface-2 border-b border-border-subtle flex items-center gap-2">
                <AlertTriangle size={12} className="text-amber-400" />
                <span className="text-2xs font-bold uppercase tracking-wider text-text-secondary">
                    Findings ({blockFindings.length})
                </span>
            </div>
            <div className="divide-y divide-border-subtle">
                {blockFindings.map(f => {
                    const Icon = severityIcon[f.severity] ?? Info
                    return (
                        <div key={f.id} className="px-3 py-2 hover:bg-surface-2/50 transition-colors">
                            <div className="flex items-start gap-2">
                                <Icon size={12} className={clsx('mt-0.5 shrink-0', severityColor[f.severity] ?? 'text-text-tertiary')} />
                                <div className="flex-1 min-w-0">
                                    <div className="flex items-center gap-2">
                                        <span className="text-xs text-text-primary font-medium truncate">{f.title}</span>
                                        {f.category && (
                                            <span className={`text-2xs font-bold uppercase tracking-wider px-1 py-0.5 rounded ${categoryBadgeClass(f.category)}`}>
                                                {f.category}
                                            </span>
                                        )}
                                    </div>
                                    {f.description && (
                                        <p className="text-2xs text-text-tertiary mt-0.5 leading-relaxed line-clamp-2">{f.description}</p>
                                    )}
                                    {f.suggestion && (
                                        <p className="text-2xs text-emerald-400/80 mt-1 line-clamp-1">{f.suggestion}</p>
                                    )}
                                    <div className="flex items-center gap-2 mt-1.5">
                                        <span className="text-2xs text-text-disabled font-mono">{f.ruleId}</span>
                                        <button
                                            onClick={() => handleFixWithAI(f)}
                                            className="flex items-center gap-1 text-2xs text-brand-400 hover:text-brand-300 transition-colors"
                                        >
                                            <Sparkles size={9} />
                                            Fix with AI
                                        </button>
                                    </div>
                                </div>
                            </div>
                        </div>
                    )
                })}
            </div>
            <button
                onClick={() => { setInspectorTab('findings'); setInspectorCollapsed(false) }}
                className="w-full px-3 py-1.5 text-2xs text-text-tertiary hover:text-text-secondary hover:bg-surface-2 border-t border-border-subtle transition-colors"
            >
                View all findings →
            </button>
        </div>
    )
}
