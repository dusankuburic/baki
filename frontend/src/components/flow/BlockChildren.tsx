import type {ReactNode} from 'react'
import type {BlockType} from '@/types/domain'
import {getBlockColor} from '@/lib/blocks'

type BlockChildrenProps = {
    children: ReactNode
    blockType: BlockType
}

export default function BlockChildren({children, blockType}: BlockChildrenProps) {
    const color = getBlockColor(blockType)

    return (
        <div className="relative pl-3">
            <div
                className="absolute top-0 bottom-0 w-0.5 rounded-full opacity-40"
                style={{left: 5, backgroundColor: color}}
            />
            <div className="flex flex-col gap-1">
                {children}
            </div>
        </div>
    )
}
