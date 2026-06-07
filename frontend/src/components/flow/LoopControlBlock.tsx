import {RotateCcw, CornerUpRight} from 'lucide-react'
import {getBlockColor} from '@/lib/blocks'
import type {Block} from '@/types/domain'

type Props = {
    block: Block
    selected?: boolean
    onClick?: () => void
}

// EXIT LOOP and NEXT LOOP render as compact loop-control chips rather than full
// BlockCards. They're visually grouped with the loop they belong to via the loop
// accent color, but are clearly distinct from regular action blocks.
export default function LoopControlBlock({block, selected = false, onClick}: Props) {
    const isExit = block.rawType === 'EXIT_LOOP'

    // EXIT LOOP uses a warm warning accent so it reads as "early exit / danger";
    // NEXT LOOP uses the loop color so it reads as "still inside the loop".
    const loopColor = getBlockColor('LOOP')
    const color = isExit ? 'var(--block-condition)' : loopColor

    const Icon = isExit ? CornerUpRight : RotateCcw
    const label = isExit ? 'Exit Loop' : 'Next Loop'
    const stripeWidth = selected ? 5 : 3

    return (
        <div
            className="relative flex items-center gap-2.5 max-w-[450px] w-full cursor-pointer transition-all duration-fast rounded-md overflow-hidden"
            style={{
                paddingTop: '6px',
                paddingBottom: '6px',
                paddingRight: '12px',
                paddingLeft: `${12 + stripeWidth}px`,
                border: '1px solid',
                borderColor: selected ? color : 'var(--border-default)',
                backgroundColor: selected
                    ? `color-mix(in srgb, ${color} 8%, var(--surface-1))`
                    : 'var(--surface-1)',
                opacity: 0.85,
            }}
            onClick={onClick}
        >
            {/* Left accent stripe — same pattern as BlockCard */}
            <div
                className="absolute left-0 top-0 bottom-0"
                style={{
                    width: stripeWidth,
                    backgroundColor: color,
                }}
            />

            <Icon
                size={13}
                style={{color, flexShrink: 0}}
                strokeWidth={2.5}
            />

            <span
                className="text-[11px] font-semibold uppercase tracking-widest"
                style={{color}}
            >
                {label}
            </span>

            <span className="text-2xs text-text-tertiary ml-auto">
                L{block.lineNumber}
            </span>
        </div>
    )
}
