import {CollapsibleSection} from './CollapsibleSection'
import type {Block} from '@/types/domain'

type BlockMetadataProps = {
    block: Block
    subflowName?: string
}

export default function BlockMetadata({block, subflowName}: BlockMetadataProps) {
    return (
        <CollapsibleSection title="Location">
            <div className="space-y-2">
                {subflowName && (
                    <div className="flex gap-2">
                        <span className="text-xs text-text-tertiary font-medium w-2/5">Subflow</span>
                        <span className="text-sm text-text-primary">{subflowName}</span>
                    </div>
                )}
                <div className="flex gap-2">
                    <span className="text-xs text-text-tertiary font-medium w-2/5">Line</span>
                    <span className="text-sm text-text-primary">{block.lineNumber}</span>
                </div>
                <div className="flex gap-2">
                    <span className="text-xs text-text-tertiary font-medium w-2/5">Depth</span>
                    <span className="text-sm text-text-primary">{block.indent} levels</span>
                </div>
            </div>
        </CollapsibleSection>
    )
}
