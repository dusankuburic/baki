import {useState, useMemo} from 'react'
import {ArrowRight, Edit3, Eye, Info, Clock} from 'lucide-react'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useFlowStore} from '@/stores/flowStore'
import clsx from 'clsx'
import VariableLineageGraph from './VariableLineageGraph'

export default function VariableLineageInInspector() {
  const history = useAnalysisStore(s => s.variableLineage)
  const navigateToBlock = useFlowStore(s => s.navigateToBlock)
  const document = useFlowStore(s => s.document)
  const [filters, setFilters] = useState<Set<string>>(new Set(['init', 'mutate', 'read']))

  const subflowNameMap = useMemo(() => {
    const map = new Map<string, string>()
    if (document) {
      for (const sf of document.subflows) map.set(sf.id, sf.name)
    }
    return map
  }, [document])

  const toggleFilter = (type: string) => {
    const next = new Set(filters)
    if (next.has(type)) {
      next.delete(type)
    } else {
      next.add(type)
    }
    setFilters(next)
  }

  const filteredEvents = useMemo(() => {
    if (!history) return []
    return history.events.filter(e => filters.has(e.type))
  }, [history, filters])

  if (!history) return null

  return (
    <div className="mt-4 border-t border-border-subtle pt-4 pb-2">
      <div className="flex items-center justify-between mb-2">
        <h3 className="text-[11px] font-bold uppercase tracking-wider text-text-tertiary flex items-center gap-1.5">
          <Clock size={12} />
          Lineage: <span className="text-brand-500 font-mono">%{history.name}%</span>
        </h3>
        <button
          onClick={() => useAnalysisStore.getState().setVariableLineage(null)}
          className="text-2xs text-text-tertiary hover:text-text-secondary transition-colors"
        >
          Clear
        </button>
      </div>

      <div className="flex items-center gap-2 mb-4">
        {[
          {id: 'init', label: 'Inits', color: 'text-green-500', bg: 'bg-green-500/10'},
          {id: 'mutate', label: 'Writes', color: 'text-amber-500', bg: 'bg-amber-500/10'},
          {id: 'read', label: 'Reads', color: 'text-blue-500', bg: 'bg-blue-500/10'},
        ].map(f => (
          <button
            key={f.id}
            onClick={() => toggleFilter(f.id)}
            className={clsx(
              'text-2xs font-bold px-2 py-0.5 rounded-full border transition-all duration-fast',
              filters.has(f.id)
                ? `${f.bg} ${f.color} border-transparent`
                : 'bg-transparent text-text-disabled border-border-subtle hover:text-text-tertiary',
            )}
          >
            {f.label}
          </button>
        ))}
      </div>

      <VariableLineageGraph events={filteredEvents} />

      <div className="space-y-3 relative before:absolute before:left-[11px] before:top-2 before:bottom-2 before:w-px before:bg-border-subtle">
        {filteredEvents.map((event, i) => (
          <div key={i} className="relative pl-7 group">
            <div
              className={clsx(
                'absolute left-0 top-1 w-5 h-5 rounded-full flex items-center justify-center border bg-surface-2 z-10',
                event.type === 'init' && 'border-green-500 text-green-500',
                event.type === 'mutate' && 'border-amber-500 text-amber-500',
                event.type === 'read' && 'border-blue-500 text-blue-500',
              )}
            >
              {event.type === 'init' && <Edit3 size={10} />}
              {event.type === 'mutate' && <ArrowRight size={10} />}
              {event.type === 'read' && <Eye size={10} />}
            </div>

            <div
              className="p-2.5 rounded-lg border border-border-subtle bg-surface-1 hover:border-brand-500/50 hover:bg-surface-2 cursor-pointer transition-all duration-fast"
              onClick={() => navigateToBlock(event.blockId)}
            >
              <div className="flex items-center justify-between mb-1">
                <span
                  className={clsx(
                    'text-2xs font-bold uppercase tracking-tighter',
                    event.type === 'init' && 'text-green-500',
                    event.type === 'mutate' && 'text-amber-500',
                    event.type === 'read' && 'text-blue-500',
                  )}
                >
                  {event.type}
                </span>
                <span className="text-2xs font-mono text-text-tertiary">L{event.line}</span>
              </div>
              <div className="text-2xs text-text-secondary truncate">
                {subflowNameMap.get(event.subflowId) ?? event.subflowId}
              </div>
            </div>
          </div>
        ))}

        {filteredEvents.length === 0 && (
          <div className="py-6 text-center opacity-40">
            <Info size={20} className="mx-auto mb-1.5" />
            <p className="text-[11px]">No events match filters.</p>
          </div>
        )}
      </div>
    </div>
  )
}
