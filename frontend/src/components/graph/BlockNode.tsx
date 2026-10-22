import {memo} from 'react'
import {Position, type Node, type NodeProps} from '@xyflow/react'
import FlowHandles from './flow/FlowHandles'
import {blockColor, useFlowColors} from './flow/colors'
import {BLOCK_NODE_HEIGHT, BLOCK_NODE_TYPE, BLOCK_NODE_WIDTH, type BlockNodeData} from './graphElements'

export type BlockFlowNode = Node<BlockNodeData, typeof BLOCK_NODE_TYPE>

/**
 * A block in the Graph view. The type's colour is the card's border, as it was in the Cytoscape
 * stylesheet; every other state (selection, findings, the variable highlight, a collaborator's
 * selection) is a class on the node wrapper, styled in `index.css`.
 */
const BlockNode = memo(function BlockNode({data}: NodeProps<BlockFlowNode>) {
  const colors = useFlowColors()
  return (
    <div className="flow-node" style={{width: BLOCK_NODE_WIDTH, height: BLOCK_NODE_HEIGHT}}>
      <FlowHandles source={Position.Bottom} target={Position.Top} />
      <div
        className="flow-card relative flex h-full w-full items-center justify-center rounded-lg border-2 bg-surface-2 px-3 text-center"
        style={{borderColor: blockColor(colors, data.type)}}
      >
        <span className="line-clamp-4 text-xs whitespace-pre-line break-words text-text-primary">{data.label}</span>
        {!!data.findingCount && (
          <span className="absolute -top-2 -right-2 rounded-full border border-border-default bg-surface-1 px-1.5 text-2xs font-semibold text-text-secondary tabular-nums">
            ⚠ {data.findingCount}
          </span>
        )}
      </div>
    </div>
  )
})

export default BlockNode
