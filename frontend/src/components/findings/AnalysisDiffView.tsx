import {useTranslation} from 'react-i18next'
import {useState} from 'react'
import clsx from 'clsx'
import {ChevronRight, GitCompareArrows, PlusCircle, CheckCircle2, MinusCircle, ArrowLeft} from 'lucide-react'
import type {AnalysisDiff, Finding} from '@/types'
import type {BlockLookup} from '@/lib/tree'
import FindingCard from './FindingCard'

interface Props {
  diff: AnalysisDiff
  blockLookup: BlockLookup
  onBack: () => void
  onFixWithAI?: (finding: Finding) => void
}

interface SectionDef {
  key: 'added' | 'removed' | 'persisted'
  label: string
  hint: string
  icon: typeof PlusCircle
  accent: string
  findings: Finding[]
  defaultOpen: boolean
}

function DiffSection({
  def,
  blockLookup,
  onFixWithAI,
}: {
  def: SectionDef
  blockLookup: BlockLookup
  onFixWithAI?: (f: Finding) => void
}) {
  const {t} = useTranslation('findings')
  const [open, setOpen] = useState(def.defaultOpen)
  const Icon = def.icon

  return (
    <div className={clsx('border-b border-border-subtle border-l-2', def.accent)}>
      <button
        onClick={() => setOpen(o => !o)}
        aria-expanded={open}
        className="w-full px-3 py-2.5 flex items-center gap-2 text-left hover:bg-surface-2 transition-colors"
      >
        <ChevronRight
          size={14}
          className={clsx('shrink-0 text-text-tertiary transition-transform duration-fast', open && 'rotate-90')}
        />
        <Icon size={14} className="shrink-0" />
        <span className="text-sm font-medium text-text-primary">{def.label}</span>
        <span className="text-2xs font-bold text-text-tertiary tabular-nums">{def.findings.length}</span>
        <span className="text-2xs text-text-tertiary ml-auto">{def.hint}</span>
      </button>
      {open && def.findings.length === 0 && (
        <div className="px-9 pb-2.5 text-2xs text-text-tertiary">{t('diff.none')}</div>
      )}
      {open &&
        def.findings.map(f => (
          <FindingCard
            key={`${def.key}-${f.id}`}
            finding={f}
            blockLookup={blockLookup}
            onFixWithAI={def.key !== 'removed' ? onFixWithAI : undefined}
          />
        ))}
    </div>
  )
}

// AnalysisDiffView shows the findings-level delta between the previous and
// current analysis run: what's new, what got fixed, what persists.
export default function AnalysisDiffView({diff, blockLookup, onBack, onFixWithAI}: Props) {
  const {t} = useTranslation('findings')
  const sections: SectionDef[] = [
    {
      key: 'added',
      label: 'New',
      hint: 'introduced since last run',
      icon: PlusCircle,
      accent: 'border-l-semantic-error [&_svg]:text-semantic-error',
      findings: diff.added,
      defaultOpen: true,
    },
    {
      key: 'removed',
      label: t('diff.fixed'),
      hint: 'no longer reported',
      icon: CheckCircle2,
      accent: 'border-l-semantic-success [&_svg]:text-semantic-success',
      findings: diff.removed,
      defaultOpen: true,
    },
    {
      key: 'persisted',
      label: t('diff.persisted'),
      hint: 'unchanged',
      icon: MinusCircle,
      accent: 'border-l-border-strong [&_svg]:text-text-tertiary',
      findings: diff.persisted,
      defaultOpen: false,
    },
  ]

  return (
    <div className="flex-1 overflow-y-auto flex flex-col">
      <div className="px-3 py-2 flex items-center gap-2 border-b border-border-subtle bg-surface-1">
        <button
          onClick={onBack}
          className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-primary px-1.5 py-1 rounded hover:bg-surface-3 transition-colors"
        >
          <ArrowLeft size={12} />
          Findings
        </button>
        <GitCompareArrows size={14} className="text-brand-400 ml-1" />
        <span className="text-xs font-medium text-text-primary">vs previous run</span>
        <span className="text-2xs text-text-tertiary ml-auto tabular-nums">
          +{diff.addedCount} / −{diff.removedCount} / ={diff.persistedCount}
        </span>
      </div>

      {!diff.hasPrevious ? (
        <div className="flex flex-col items-center justify-center flex-1 gap-2 p-6 text-center">
          <GitCompareArrows size={22} className="text-text-tertiary" />
          <span className="text-sm text-text-secondary">{t('diff.firstAnalysis')}</span>
          <span className="text-2xs text-text-tertiary">
            Change the flow and re-analyze — this view will then show which findings are new, fixed, or unchanged.
          </span>
        </div>
      ) : (
        <div>
          {sections.map(s => (
            <DiffSection key={s.key} def={s} blockLookup={blockLookup} onFixWithAI={onFixWithAI} />
          ))}
        </div>
      )}
    </div>
  )
}
