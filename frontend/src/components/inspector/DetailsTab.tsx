import {Box} from 'lucide-react'
import {CollapsibleSection} from './CollapsibleSection'
import DetailsHeader from './DetailsHeader'
import PropertiesTable from './PropertiesTable'
import VariableChips from './VariableChips'
import BlockMetadata from './BlockMetadata'
import ChildrenList from './ChildrenList'
import VariableLineageInInspector from './VariableLineageInInspector'
import BlockFindings from './BlockFindings'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {analysisApi} from '@/api'
import {findBlockInDoc} from '@/lib/tree'
import {logger} from '@/lib/logger'
import type {VariableHistory} from '@/types'

export default function DetailsTab() {
    const document = useFlowStore(s => s.document)
    const selectedBlockId = useFlowStore(s => s.selectedBlockId)

    if (!document || !selectedBlockId) {
        return (
            <div className="flex flex-col items-center justify-center py-16 text-center">
                <div className="w-12 h-12 rounded-full bg-surface-2 flex items-center justify-center mb-3">
                    <Box size={24} className="text-text-tertiary" />
                </div>
                <div className="text-sm font-medium text-text-primary mb-1">Select a block</div>
                <div className="text-xs text-text-tertiary max-w-48">
                    Click any block in the flow to inspect its properties and metadata.
                </div>
            </div>
        )
    }

    const result = findBlockInDoc(document, selectedBlockId)
    if (!result) return null
    const {block, subflowName} = result

    const handleVariableClick = async (name: string) => {
        if (!document) return
        try {
            const h = await analysisApi.getVariableLineage(name)
            useAnalysisStore.getState().setVariableLineage(h as unknown as VariableHistory)
        } catch (err) {
            logger.warn('Failed to get lineage:', err)
        }
    }

    return (
        <div className="flex flex-col h-full overflow-y-auto custom-scrollbar">
            <DetailsHeader block={block} />
            <div className="p-4 space-y-4">
                <PropertiesTable properties={block.properties || {}} />
                <BlockFindings />
                {block.variables?.length > 0 && (
                    <CollapsibleSection title={`Variables used (${block.variables.length})`}>
                        <VariableChips variables={block.variables} onVariableClick={handleVariableClick} />
                    </CollapsibleSection>
                )}
                {/* Lineage panel is always rendered when data is available — it handles its
                    own null-check internally. Keeping it outside the variables conditional
                    ensures it shows even when a block only has an _output (no variables used). */}
                <VariableLineageInInspector />
                <BlockMetadata block={block} subflowName={subflowName} />
                {block.children?.length > 0 && (
                    <CollapsibleSection title={`Children (${block.children.length})`}>
                        <ChildrenList
                            children={block.children}
                            onSelect={(id) => useFlowStore.getState().selectBlock(id)}
                        />
                    </CollapsibleSection>
                )}
            </div>
        </div>
    )
}
