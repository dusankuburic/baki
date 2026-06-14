import {useMemo} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {analysisApi} from '@/api'
import type {VariableHistory, Block} from '@/types'
import clsx from 'clsx'
import {Variable} from 'lucide-react'
import {logger} from '@/lib/logger'

export default function VariablesTab() {
  const document = useFlowStore(s => s.document)
  const selectedSubflowId = useFlowStore(s => s.selectedSubflowId)
  const selectedVariable = useUIStore(s => s.selectedVariable)
  const setSelectedVariable = useUIStore(s => s.setSelectedVariable)
  const setVariableLineage = useAnalysisStore(s => s.setVariableLineage)

  const allVariables = useMemo(() => {
    if (!document) return []
    const subflow = document.subflows.find(s => s.id === selectedSubflowId) ?? document.subflows[0]
    if (!subflow) return []
    const vars = new Set<string>()
    const walk = (blocks: Block[]) => {
      for (const b of blocks) {
        if (b.variables) {
          for (const v of b.variables) vars.add(v)
        }
        if (b.children) walk(b.children)
      }
    }
    walk(subflow.blocks)
    return Array.from(vars).sort((a, b) => a.toLowerCase().localeCompare(b.toLowerCase()))
  }, [document, selectedSubflowId])

  const handleSelect = async (v: string) => {
    if (v === selectedVariable) {
      setSelectedVariable(null)
      return
    }
    setSelectedVariable(v)
    try {
      const h = await analysisApi.getVariableLineage(v)
      setVariableLineage(h as unknown as VariableHistory)
      useUIStore.getState().setVariablePanelOpen(true)
    } catch (err) {
      logger.warn(err)
    }
  }

  if (!document) {
    return (
      <div className="flex-1 flex flex-col items-center justify-center p-4 text-center text-text-tertiary">
        <Variable size={24} className="mb-2 opacity-50" />
        <p className="text-sm">No flow loaded</p>
      </div>
    )
  }

  return (
    <div className="flex-1 overflow-y-auto custom-scrollbar p-2 space-y-1 bg-surface-1">
      <div className="px-2 py-2 text-xs font-semibold text-text-tertiary uppercase tracking-wider sticky top-0 bg-surface-1 z-10 backdrop-blur">
        Variables ({allVariables.length})
      </div>
      {allVariables.length === 0 ? (
        <div className="text-sm text-text-tertiary px-2 py-4">No variables found</div>
      ) : (
        allVariables.map(v => (
          <button
            key={v}
            onClick={() => handleSelect(v)}
            className={clsx(
              'w-full text-left px-3 py-1.5 rounded text-sm font-mono truncate transition-colors flex items-center gap-2',
              selectedVariable === v
                ? 'bg-block-variable-bg text-block-variable ring-1 ring-block-variable/30'
                : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary'
            )}
          >
            <span className="opacity-50 text-xs">%</span>
            {v}
            <span className="opacity-50 text-xs">%</span>
          </button>
        ))
      )}
    </div>
  )
}
