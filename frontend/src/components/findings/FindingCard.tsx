import type {Finding, FlowDocument} from '@/types/domain'
import {useFlowStore} from '@/stores/flowStore'
import {ArrowRight, Sparkles} from 'lucide-react'

interface Props {
  finding: Finding
  doc: FlowDocument
  onFixWithAI?: (finding: Finding) => void
}

function findBlock(doc: FlowDocument, blockId: string) {
  for (const sf of doc.subflows) {
    const stack = [...sf.blocks]
    while (stack.length) {
      const b = stack.pop()!
      if (b.id === blockId) return {block: b, subflowName: sf.name}
      if (b.children?.length) stack.push(...b.children)
    }
  }
  return null
}

export default function FindingCard({finding, doc, onFixWithAI}: Props) {
  const selectBlock = useFlowStore(s => s.selectBlock)
  const selectSubflow = useFlowStore(s => s.selectSubflow)

  const handleJump = () => {
    selectSubflow(finding.subflowId)
    selectBlock(finding.blockId)
  }

  const loc = findBlock(doc, finding.blockId)
  const blockLabel = loc?.block.name ?? finding.blockId.slice(0, 8)
  const subflowLabel = loc?.subflowName

  return (
    <div className="px-4 py-2 pl-9 flex items-center gap-3 hover:bg-surface-2/50 transition-colors group">
      <div className="flex-1 min-w-0">
        <span className="text-xs text-text-primary font-mono truncate">{blockLabel}</span>
        {subflowLabel && (
          <span className="text-2xs text-text-tertiary ml-2">in {subflowLabel}</span>
        )}
      </div>

      <button
        onClick={handleJump}
        className="flex items-center gap-1 text-2xs text-text-tertiary hover:text-text-primary px-1.5 py-0.5 rounded hover:bg-surface-3 transition-colors shrink-0"
      >
        <ArrowRight size={10} />
        Jump
      </button>

      {onFixWithAI && (
        <button
          onClick={() => onFixWithAI(finding)}
          className="flex items-center gap-1 text-2xs text-brand-400 hover:text-brand-300 px-1.5 py-0.5 rounded hover:bg-brand-500/10 transition-colors shrink-0"
        >
          <Sparkles size={10} />
          Fix with AI
        </button>
      )}
    </div>
  )
}
