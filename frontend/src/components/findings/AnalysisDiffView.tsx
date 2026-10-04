import {useTranslation} from 'react-i18next'
import {useMemo, useState} from 'react'
import {Virtuoso} from 'react-virtuoso'
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

type SectionKey = 'added' | 'removed' | 'persisted'

interface SectionDef {
  key: SectionKey
  label: string
  hint: string
  icon: typeof PlusCircle
  accent: string
  findings: Finding[]
}

// Flattened render rows — the row model FindingsList uses, so all three
// sections go through ONE virtualizer: a header per section and, while it is
// open, one row per finding (or a single "none" row). Mounting a FindingCard
// per finding made the diff of a large flow unusable: 3,000 new findings took
// ~6 s to mount, and every one of them stayed mounted.
type Row =
  | {kind: 'header'; section: SectionDef; open: boolean; sectionEnd: boolean}
  | {kind: 'empty'; section: SectionDef}
  | {kind: 'card'; section: SectionDef; finding: Finding; sectionEnd: boolean}

function rowKey(row: Row): string {
  if (row.kind === 'header') return `h:${row.section.key}`
  if (row.kind === 'empty') return `e:${row.section.key}`
  return `${row.section.key}-${row.finding.id}`
}

// AnalysisDiffView shows the findings-level delta between the previous and
// current analysis run: what's new, what got fixed, what persists.
export default function AnalysisDiffView({diff, blockLookup, onBack, onFixWithAI}: Props) {
  const {t} = useTranslation('findings')
  // Persisted findings are the bulk of a re-run and rarely what the user came
  // for, so that section starts collapsed.
  const [closed, setClosed] = useState<Set<SectionKey>>(() => new Set(['persisted']))

  const sections = useMemo<SectionDef[]>(
    () => [
      {
        key: 'added',
        label: 'New',
        hint: 'introduced since last run',
        icon: PlusCircle,
        accent: 'border-l-semantic-error [&_svg]:text-semantic-error',
        findings: diff.added,
      },
      {
        key: 'removed',
        label: t('diff.fixed'),
        hint: 'no longer reported',
        icon: CheckCircle2,
        accent: 'border-l-semantic-success [&_svg]:text-semantic-success',
        findings: diff.removed,
      },
      {
        key: 'persisted',
        label: t('diff.persisted'),
        hint: 'unchanged',
        icon: MinusCircle,
        accent: 'border-l-border-strong [&_svg]:text-text-tertiary',
        findings: diff.persisted,
      },
    ],
    [diff, t],
  )

  const rows = useMemo<Row[]>(() => {
    const out: Row[] = []
    for (const section of sections) {
      const open = !closed.has(section.key)
      out.push({kind: 'header', section, open, sectionEnd: !open})
      if (!open) continue
      if (section.findings.length === 0) {
        out.push({kind: 'empty', section})
        continue
      }
      const last = section.findings.length - 1
      section.findings.forEach((finding, i) => {
        out.push({kind: 'card', section, finding, sectionEnd: i === last})
      })
    }
    return out
  }, [sections, closed])

  const toggle = (key: SectionKey) => {
    setClosed(prev => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }

  return (
    <div className="flex-1 min-h-0 flex flex-col">
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
          <span className="text-2xs text-text-tertiary">{t('diffView.empty')}</span>
        </div>
      ) : (
        <div className="flex-1 min-h-0">
          <Virtuoso
            style={{height: '100%'}}
            data={rows}
            computeItemKey={(_index, row) => rowKey(row)}
            itemContent={(_index, row) => {
              const sectionEnd = row.kind === 'empty' || row.sectionEnd
              const frame = clsx('border-l-2', row.section.accent, sectionEnd && 'border-b border-border-subtle')
              if (row.kind === 'header') {
                const {section, open} = row
                const Icon = section.icon
                return (
                  <div className={frame}>
                    <button
                      onClick={() => toggle(section.key)}
                      aria-expanded={open}
                      className="w-full px-3 py-2.5 flex items-center gap-2 text-left hover:bg-surface-2 transition-colors"
                    >
                      <ChevronRight
                        size={14}
                        className={clsx(
                          'shrink-0 text-text-tertiary transition-transform duration-fast',
                          open && 'rotate-90',
                        )}
                      />
                      <Icon size={14} className="shrink-0" />
                      <span className="text-sm font-medium text-text-primary">{section.label}</span>
                      <span className="text-2xs font-bold text-text-tertiary tabular-nums">
                        {section.findings.length}
                      </span>
                      <span className="text-2xs text-text-tertiary ml-auto">{section.hint}</span>
                    </button>
                  </div>
                )
              }
              if (row.kind === 'empty') {
                return (
                  <div className={frame}>
                    <div className="px-9 pb-2.5 text-2xs text-text-tertiary">{t('diff.none')}</div>
                  </div>
                )
              }
              return (
                <div className={frame}>
                  <FindingCard
                    finding={row.finding}
                    blockLookup={blockLookup}
                    onFixWithAI={row.section.key !== 'removed' ? onFixWithAI : undefined}
                  />
                </div>
              )
            }}
          />
        </div>
      )}
    </div>
  )
}
