import {useMemo} from 'react'
import type {VariableEvent} from '@/types/domain'
import {useFlowStore} from '@/stores/flowStore'
import {Info} from 'lucide-react'

const EVENT_COLOR: Record<VariableEvent['type'], string> = {
    init:   '#22c55e',
    mutate: '#f59e0b',
    read:   '#3b82f6',
}

const EVENT_PATH: Record<VariableEvent['type'], string> = {
    // pencil icon
    init:   'M3 17.25V21h3.75L17.81 9.94l-3.75-3.75L3 17.25zM20.71 7.04a1 1 0 0 0 0-1.41l-2.34-2.34a1 1 0 0 0-1.41 0l-1.83 1.83 3.75 3.75 1.83-1.83z',
    // arrow icon
    mutate: 'M5 12h14M12 5l7 7-7 7',
    // eye icon
    read:   'M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z M12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6z',
}

type Props = {
    events: VariableEvent[]
}

export default function VariableLineageGraph({events}: Props) {
    const navigateToBlock = useFlowStore(s => s.navigateToBlock)

    const nodes = useMemo(() => {
        return events.map((e, i) => ({
            ...e,
            id: `v-${i}`,
            x: i * 80 + 40,
            y: 40,
        }))
    }, [events])

    if (nodes.length === 0) {
        return (
            <div className="flex flex-col items-center justify-center py-6 opacity-40">
                <Info size={20} className="mb-1.5" />
                <p className="text-[11px]">No events match the active filters.</p>
            </div>
        )
    }

    const width = nodes.length * 80 + 80

    return (
        <div className="w-full overflow-x-auto custom-scrollbar pb-4">
            <svg width={width} height={100} className="mx-auto">
                <defs>
                    <marker
                        id="vl-arrow"
                        markerWidth="8"
                        markerHeight="6"
                        refX="7"
                        refY="3"
                        orient="auto"
                    >
                        <polygon points="0 0, 8 3, 0 6" fill="currentColor" className="text-border-default" />
                    </marker>
                </defs>

                {/* Edges */}
                {nodes.slice(1).map((node, i) => (
                    <line
                        key={`edge-${i}`}
                        x1={nodes[i].x + 14}
                        y1={nodes[i].y}
                        x2={node.x - 14}
                        y2={node.y}
                        stroke="currentColor"
                        strokeWidth="1.5"
                        strokeDasharray={node.type === 'read' ? '4 2' : undefined}
                        className="text-border-subtle"
                        markerEnd="url(#vl-arrow)"
                    />
                ))}

                {/* Nodes */}
                {nodes.map((node) => {
                    const color = EVENT_COLOR[node.type]
                    return (
                        <g
                            key={node.id}
                            className="cursor-pointer"
                            onClick={() => navigateToBlock(node.blockId)}
                        >
                            <circle
                                cx={node.x}
                                cy={node.y}
                                r="16"
                                fill="var(--bg-surface-2)"
                                stroke={color}
                                strokeWidth="2"
                            />
                            {/* Icon via SVG path — avoids foreignObject overhead */}
                            <g
                                transform={`translate(${node.x - 6}, ${node.y - 6})`}
                                fill="none"
                                stroke={color}
                                strokeWidth="1.5"
                                strokeLinecap="round"
                                strokeLinejoin="round"
                                viewBox="0 0 24 24"
                            >
                                <svg width="12" height="12" viewBox="0 0 24 24">
                                    <path d={EVENT_PATH[node.type]} />
                                </svg>
                            </g>
                            <text
                                x={node.x}
                                y={node.y + 28}
                                textAnchor="middle"
                                className="text-[9px] fill-text-tertiary font-mono"
                            >
                                L{node.line}
                            </text>
                        </g>
                    )
                })}
            </svg>
        </div>
    )
}
