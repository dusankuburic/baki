import {memo} from 'react'
import {useTranslation} from 'react-i18next'
import {AlertCircle, AlertTriangle, Check} from 'lucide-react'
import {Position, type Node, type NodeProps} from '@xyflow/react'
import FlowHandles from '@/components/graph/flow/FlowHandles'
import {roleConfig} from './roles'
import {NODE_CARD_HEIGHT, NODE_CARD_WIDTH} from './layout'
import type {EnhancedGraphNode} from './model'

export const SUBFLOW_CARD_TYPE = 'subflowCard'

/**
 * A card's own data. Written as a type, not an interface, so React Flow accepts it as node data.
 */
export type SubflowCardData = {
  subflow: EnhancedGraphNode
  /** The subflow the Local Map is opened on. */
  anchor: boolean
}

export type SubflowCardNode = Node<SubflowCardData, typeof SUBFLOW_CARD_TYPE>

/**
 * The subflow map's node: plain DOM, where it used to be an SVG data URI painted into Cytoscape's
 * canvas as a background image. Nothing here loads, so a theme switch or a highlight is a CSS
 * repaint and never a re-rasterized card.
 */
const SubflowCard = memo(function SubflowCard({
  data,
  sourcePosition = Position.Bottom,
  targetPosition = Position.Top,
}: NodeProps<SubflowCardNode>) {
  const {t} = useTranslation('flow')
  const {subflow, anchor} = data
  const role = roleConfig(subflow.role)

  return (
    <div className="flow-node" style={{width: NODE_CARD_WIDTH, height: NODE_CARD_HEIGHT}}>
      <FlowHandles source={sourcePosition} target={targetPosition} />
      <div className="flow-card relative flex h-full w-full overflow-hidden rounded-xl border-2 border-border-default bg-surface-1">
        <span className="w-[5px] shrink-0" style={{backgroundColor: role.color}} />
        <div className="flex min-w-0 flex-1 flex-col justify-center gap-0.5 px-2.5 py-1.5">
          <div className="flex items-center gap-1.5">
            <span
              className="rounded border px-1.5 text-[9px] font-bold tracking-wide"
              style={{
                color: role.color,
                backgroundColor: `color-mix(in srgb, ${role.color} 14%, transparent)`,
                borderColor: `color-mix(in srgb, ${role.color} 40%, transparent)`,
              }}
            >
              {t(role.badgeKey)}
            </span>
            <span className="text-[10px] text-text-tertiary">
              {t('graph.blocksShort', {blocks: subflow.blockCount || 0})}
            </span>
            {anchor && (
              <span className="ml-auto rounded bg-brand-500 px-1.5 text-[8px] font-bold tracking-wide text-white">
                {t('graph.thisSubflow')}
              </span>
            )}
          </div>
          <div className="truncate text-[13px] font-semibold text-text-primary" title={subflow.label || subflow.id}>
            {subflow.label || subflow.id}
          </div>
          <div className="flex items-center gap-2 text-[10px] text-text-tertiary">
            <span>{t('graph.degreeSummary', {in: subflow.inDegree, out: subflow.outDegree})}</span>
            <HealthChip errorCount={subflow.errorCount} warnCount={subflow.warnCount} />
          </div>
        </div>
      </div>
    </div>
  )
})

function HealthChip({errorCount, warnCount}: {errorCount: number; warnCount: number}) {
  const {t} = useTranslation('flow')
  if (errorCount > 0) {
    return (
      <span
        className="ml-auto flex items-center gap-0.5 rounded-full border border-semantic-error/60 bg-semantic-error/15 px-1.5 font-bold text-semantic-error"
        title={t('graph.errorsCount', {count: errorCount})}
      >
        <AlertCircle size={9} /> {errorCount}
      </span>
    )
  }
  if (warnCount > 0) {
    return (
      <span
        className="ml-auto flex items-center gap-0.5 rounded-full border border-semantic-warning/60 bg-semantic-warning/15 px-1.5 font-bold text-semantic-warning"
        title={t('graph.warningsCount', {count: warnCount})}
      >
        <AlertTriangle size={9} /> {warnCount}
      </span>
    )
  }
  return (
    <span
      className="ml-auto flex items-center rounded-full border border-semantic-success/60 bg-semantic-success/15 px-1 font-bold text-semantic-success"
      title={t('graph.cleanNoIssues')}
    >
      <Check size={9} />
    </span>
  )
}

export default SubflowCard
