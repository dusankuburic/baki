import {useTranslation} from 'react-i18next'
import {AlertTriangle, ChevronDown, ChevronRight} from 'lucide-react'
import {useState} from 'react'
import clsx from 'clsx'
import {Button, Modal} from '@/components/shared'
import type {ContextPreview} from '@/types'

interface Props {
  preview: ContextPreview
  onClose: () => void
  onConfirm: () => void
}

export default function ContextPreviewModal({preview, onClose, onConfirm}: Props) {
  const {t} = useTranslation('chat')
  const [expandSystem, setExpandSystem] = useState(false)
  const [expandContext, setExpandContext] = useState(true)

  const ratio = preview.contextLimit > 0 ? preview.estimatedTokens / preview.contextLimit : 0
  const usage = (ratio * 100).toFixed(1)
  const overLimit = ratio > 1
  const nearLimit = !overLimit && ratio >= 0.9

  return (
    <Modal
      isOpen
      onClose={onClose}
      title={t('preview.title')}
      size="lg"
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            {t('preview.cancel')}
          </Button>
          <Button variant={overLimit ? 'danger' : 'primary'} size="sm" onClick={onConfirm}>
            {t('preview.send')}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <div
          className={clsx(
            'flex items-center justify-between gap-2 text-xs tabular-nums rounded-lg px-2.5 py-1.5',
            overLimit
              ? 'bg-semantic-error/10 text-semantic-error'
              : nearLimit
                ? 'bg-semantic-warning/10 text-semantic-warning'
                : 'text-text-secondary',
          )}
        >
          <span className="flex items-center gap-1.5 min-w-0">
            {(overLimit || nearLimit) && <AlertTriangle size={12} className="shrink-0" />}
            <span className="truncate">{t('preview.estimated', {count: preview.estimatedTokens})}</span>
          </span>
          <span className="shrink-0">{t('preview.limit', {limit: preview.contextLimit.toLocaleString(), usage})}</span>
        </div>

        {overLimit && <p className="text-xs text-semantic-error">{t('preview.overLimit')}</p>}

        {/* The parts are absent from a server that predates them (a rolling deploy): then only the total shows. */}
        <div className="space-y-0.5 text-xs text-text-tertiary tabular-nums">
          {preview.breakdown && (
            <p>
              {t('preview.breakdown', {
                system: preview.breakdown.system.toLocaleString(),
                flowContext: preview.breakdown.context.toLocaleString(),
                history: preview.breakdown.history.toLocaleString(),
                tools: preview.breakdown.tools.toLocaleString(),
                message: preview.breakdown.user.toLocaleString(),
              })}
            </p>
          )}
          {!!preview.contextBudget && <p>{t('preview.budget', {budget: preview.contextBudget.toLocaleString()})}</p>}
          {!!preview.historyDropped && (
            <p className="text-semantic-warning">
              {t('preview.historyDropped', {dropped: preview.historyDropped, total: preview.historyMessages})}
            </p>
          )}
          {preview.toolsOffered && preview.tools && preview.tools.length > 0 && (
            <p className="break-words">{t('preview.tools', {tools: preview.tools.join(', ')})}</p>
          )}
        </div>

        <CollapsibleSection
          title={t('preview.systemPrompt')}
          expanded={expandSystem}
          onToggle={() => setExpandSystem(v => !v)}
        >
          <pre className="text-xs text-text-secondary whitespace-pre-wrap font-mono bg-surface-2 rounded-lg p-3 max-h-[200px] overflow-y-auto custom-scrollbar">
            {preview.systemPrompt}
          </pre>
        </CollapsibleSection>

        <CollapsibleSection
          title={t('preview.flowContext')}
          expanded={expandContext}
          onToggle={() => setExpandContext(v => !v)}
        >
          <pre className="text-xs text-text-secondary whitespace-pre-wrap font-mono bg-surface-2 rounded-lg p-3 max-h-[300px] overflow-y-auto custom-scrollbar">
            {preview.contextText || t('preview.noContext')}
          </pre>
        </CollapsibleSection>

        <div>
          <span className="text-xs font-medium text-text-secondary">{t('preview.yourMessage')}</span>
          <div className="mt-1 text-sm text-text-primary bg-surface-2 rounded-lg p-3 whitespace-pre-wrap wrap-break-word">
            {preview.userMessage}
          </div>
        </div>
      </div>
    </Modal>
  )
}

function CollapsibleSection({
  title,
  expanded,
  onToggle,
  children,
}: {
  title: string
  expanded: boolean
  onToggle: () => void
  children: React.ReactNode
}) {
  return (
    <div>
      <button
        type="button"
        className="flex items-center gap-1 text-xs font-medium text-text-secondary hover:text-text-primary"
        onClick={onToggle}
        aria-expanded={expanded}
      >
        {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        {title}
      </button>
      {expanded && <div className="mt-1">{children}</div>}
    </div>
  )
}
