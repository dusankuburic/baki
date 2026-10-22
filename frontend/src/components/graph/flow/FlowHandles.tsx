import {Handle, Position} from '@xyflow/react'

/**
 * The anchors React Flow draws an edge's ends from. These views never connect anything by hand, so
 * the handles are invisible (`.react-flow__handle` in `index.css`) and exist only so an edge has an
 * endpoint to start and finish at.
 */
export default function FlowHandles({
  source = Position.Bottom,
  target = Position.Top,
}: {
  source?: Position
  target?: Position
}) {
  return (
    <>
      <Handle type="target" position={target} isConnectable={false} />
      <Handle type="source" position={source} isConnectable={false} />
    </>
  )
}
