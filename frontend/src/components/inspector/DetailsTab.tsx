import {useMemo, lazy, Suspense} from 'react'
import {Box} from 'lucide-react'
import {CollapsibleSection} from './CollapsibleSection'
import DetailsHeader from './DetailsHeader'
import PropertiesTable from './PropertiesTable'
import VariableChips from './VariableChips'
import BlockMetadata from './BlockMetadata'
import ChildrenList from './ChildrenList'
import BlockFindings from './BlockFindings'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {analysisApi} from '@/api'
import {findBlockInDoc} from '@/lib/tree'
import {logger} from '@/lib/logger'

// Lineage graph is lazy: it transitively imports cytoscape + cytoscape-dagre
// (~530 kB). DetailsTab is the DEFAULT inspector tab and mounts for every
// block selection — a static import would put the graph chunk in the eager
// entry graph. The component null-renders without lineage data, so we gate
// the mount on the same store value it checks internally; cytoscape is only
// fetched once a user actually requests a variable's lineage.
const VariableLineageInInspector = lazy(() => import('./VariableLineageInInspector'))

export default function DetailsTab() {
  const document = useFlowStore(s => s.document)
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)
  // Lineage presence gates the lazy mount above (mirrors the component's own
  // internal null-check on the same value).
  const hasLineage = useAnalysisStore(s => s.variableLineage != null)

  // useMemo must run unconditionally on every render (Rules of Hooks) even
  // though its result is only needed once the two early returns below are
  // passed — the null checks live inside the memo callback instead of
  // gating the hook call itself.
  const result = useMemo(
    () => (document && selectedBlockId ? findBlockInDoc(document, selectedBlockId) : null),
    [document, selectedBlockId],
  )

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

  if (!result) return null
  const {block, subflowName} = result

  const handleVariableClick = async (name: string) => {
    if (!document) return
    try {
      const h = await analysisApi.getVariableLineage(name)
      useAnalysisStore.getState().setVariableLineage(h)
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
        {/* Lineage panel mounts only when data is available (the lazy chunk
                    containing cytoscape fetches on first lineage request). Keeping it
                    outside the variables conditional ensures it shows even when a block
                    only has an _output (no variables used). */}
        {hasLineage && (
          <Suspense fallback={null}>
            <VariableLineageInInspector />
          </Suspense>
        )}
        <BlockMetadata block={block} subflowName={subflowName} />
        {block.children?.length > 0 && (
          <CollapsibleSection title={`Children (${block.children.length})`}>
            <ChildrenList children={block.children} onSelect={id => useFlowStore.getState().selectBlock(id)} />
          </CollapsibleSection>
        )}
      </div>
    </div>
  )
}
