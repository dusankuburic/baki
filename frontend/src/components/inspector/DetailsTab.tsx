import {useMemo, lazy, Suspense} from 'react'
import {useTranslation} from 'react-i18next'
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

const VariableLineageInInspector = lazy(() => import('./VariableLineageInInspector'))

export default function DetailsTab() {
  const {t} = useTranslation('inspector')
  const document = useFlowStore(s => s.document)
  const selectedBlockId = useFlowStore(s => s.selectedBlockId)
  const hasLineage = useAnalysisStore(s => s.variableLineage != null)

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
        <div className="text-sm font-medium text-text-primary mb-1">{t('details.placeholderTitle')}</div>
        <div className="text-xs text-text-tertiary max-w-48">{t('details.placeholderBody')}</div>
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
          <CollapsibleSection title={t('details.variablesUsed', {count: block.variables.length})}>
            <VariableChips variables={block.variables} onVariableClick={handleVariableClick} />
          </CollapsibleSection>
        )}
        {hasLineage && (
          <Suspense fallback={null}>
            <VariableLineageInInspector />
          </Suspense>
        )}
        <BlockMetadata block={block} subflowName={subflowName} />
        {block.children?.length > 0 && (
          <CollapsibleSection title={t('details.children', {count: block.children.length})}>
            <ChildrenList children={block.children} onSelect={id => useFlowStore.getState().selectBlock(id)} />
          </CollapsibleSection>
        )}
      </div>
    </div>
  )
}
