import React from 'react'
import {useTranslation} from 'react-i18next'
import {ArrowDownToLine, ArrowUpFromLine} from 'lucide-react'
import clsx from 'clsx'
import type {SubflowMetrics} from '@/types'

function MiniBar({value, max, color}: {value: number; max: number; color: string}) {
  const pct = max > 0 ? Math.min(100, (value / max) * 100) : 0
  return (
    <div className="h-1.5 w-full bg-surface-3 rounded-full overflow-hidden">
      <div className={clsx('h-full rounded-full transition-all duration-fast', color)} style={{width: `${pct}%`}} />
    </div>
  )
}

const CYCLO_AMBER = 10
const CYCLO_RED = 20
const COG_AMBER = 15
const COG_RED = 30

const SubflowMetricsRow = React.memo(function SubflowMetricsRow({
  m,
  onSelect,
}: {
  m: SubflowMetrics
  onSelect: () => void
}) {
  const {t} = useTranslation('inspector')
  const cycloColor =
    m.cyclomaticComplexity > CYCLO_RED
      ? 'bg-red-500'
      : m.cyclomaticComplexity > CYCLO_AMBER
        ? 'bg-amber-500'
        : 'bg-green-500'
  const cogColor =
    m.cognitiveComplexity > COG_RED ? 'bg-red-500' : m.cognitiveComplexity > COG_AMBER ? 'bg-amber-500' : 'bg-green-500'

  return (
    <button
      onClick={onSelect}
      className="w-full text-left p-3 rounded-lg border border-border-subtle bg-surface-0 hover:border-brand-500/30 hover:bg-surface-2 transition-all duration-fast"
    >
      <div className="flex items-center justify-between mb-2">
        <span className="text-xs font-medium text-text-primary truncate">{m.subflowName}</span>
        <span className="text-xs text-text-tertiary tabular-nums">{t('subflows.blocks', {count: m.blockCount})}</span>
      </div>
      <div className="grid grid-cols-2 gap-x-4 gap-y-1.5">
        <div>
          <div className="flex items-center justify-between mb-0.5">
            <span className="text-xs text-text-tertiary uppercase tracking-wider">{t('subflows.cyclomatic')}</span>
            <span className="text-xs font-mono text-text-secondary tabular-nums">{m.cyclomaticComplexity}</span>
          </div>
          <MiniBar value={m.cyclomaticComplexity} max={30} color={cycloColor} />
        </div>
        <div>
          <div className="flex items-center justify-between mb-0.5">
            <span className="text-xs text-text-tertiary uppercase tracking-wider">{t('subflows.cognitive')}</span>
            <span className="text-xs font-mono text-text-secondary tabular-nums">{m.cognitiveComplexity}</span>
          </div>
          <MiniBar value={m.cognitiveComplexity} max={40} color={cogColor} />
        </div>
      </div>
      <div className="flex items-center gap-3 mt-2 pt-2 border-t border-border-subtle">
        <span className="flex items-center gap-1 text-xs text-text-tertiary">
          <ArrowDownToLine size={12} /> {t('subflows.fanIn', {value: m.fanIn})}
        </span>
        <span className="flex items-center gap-1 text-xs text-text-tertiary">
          <ArrowUpFromLine size={12} /> {t('subflows.fanOut', {value: m.fanOut})}
        </span>
        <span className="flex items-center gap-1 text-xs text-text-tertiary">{t('subflows.depth', {value: m.maxNestingDepth})}</span>
        <span className="flex items-center gap-1 text-xs text-text-tertiary">{t('subflows.vars', {value: m.variableCount})}</span>
      </div>
    </button>
  )
})

export default SubflowMetricsRow
