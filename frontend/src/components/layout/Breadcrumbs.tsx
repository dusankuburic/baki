import {useMemo} from 'react'
import {ChevronRight, Home} from 'lucide-react'
import {useFlowStore} from '@/stores/flowStore'
import type {Block} from '@/types/domain'

export default function Breadcrumbs() {
    const document = useFlowStore(s => s.document)
    const selectedBlockId = useFlowStore(s => s.selectedBlockId)
    const selectedSubflowId = useFlowStore(s => s.selectedSubflowId)
    const selectBlock = useFlowStore(s => s.selectBlock)
    const selectSubflow = useFlowStore(s => s.selectSubflow)

    const path = useMemo(() => {
        if (!document || !selectedSubflowId) return []
        
        const subflow = document.subflows.find(s => s.id === selectedSubflowId)
        if (!subflow) return []

        const crumbs: {id: string, name: string, type: 'subflow' | 'block'}[] = [
            {id: subflow.id, name: subflow.name, type: 'subflow'}
        ]

        if (selectedBlockId) {
            const blockPath = findBlockPath(subflow.blocks, selectedBlockId)
            if (blockPath) {
                crumbs.push(...blockPath.map(b => ({
                    id: b.id,
                    name: b.name,
                    type: 'block' as const
                })))
            }
        }

        return crumbs
    }, [document, selectedSubflowId, selectedBlockId])

    if (!document || path.length === 0) return null

    return (
        <div className="flex items-center gap-1.5 px-3 h-8 text-[11px] text-text-tertiary bg-surface-1 border-b border-border-subtle overflow-hidden">
            <button 
                className="flex items-center gap-1 hover:text-text-primary transition-colors flex-shrink-0"
                onClick={() => selectSubflow(document.subflows[0].id)}
            >
                <Home size={12} />
                <span className="truncate max-w-[100px]">{document.name}</span>
            </button>
            
            {path.map((crumb, i) => (
                <div key={crumb.id} className="flex items-center gap-1.5 min-w-0">
                    <ChevronRight size={10} className="flex-shrink-0 opacity-50" />
                    <button
                        className={`hover:text-text-primary transition-colors truncate ${
                            i === path.length - 1 ? 'text-text-secondary font-medium' : ''
                        }`}
                        onClick={() => {
                            if (crumb.type === 'subflow') selectSubflow(crumb.id)
                            else selectBlock(crumb.id)
                        }}
                        title={crumb.name}
                    >
                        {crumb.name}
                    </button>
                </div>
            ))}
        </div>
    )
}

function findBlockPath(blocks: Block[], targetId: string): Block[] | null {
    for (const block of blocks) {
        if (block.id === targetId) return [block]
        if (block.children?.length) {
            const subPath = findBlockPath(block.children, targetId)
            if (subPath) return [block, ...subPath]
        }
    }
    return null
}
