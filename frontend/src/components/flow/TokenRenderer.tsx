import React from 'react'
import {useFlowStore} from '@/stores/flowStore'
import type {BlockToken, VariableHistory} from '@/types/domain'
import clsx from 'clsx'

import {analysisApi} from '@/api'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useUIStore} from '@/stores/uiStore'

import {ExternalLink, Variable as VariableIcon} from 'lucide-react'
import {Tooltip} from '@/components/shared'

interface TokenRendererProps {
  tokens: BlockToken[]
}

export default function TokenRenderer({tokens}: TokenRendererProps) {
  return (
    <div className="flex flex-wrap items-center">
      {tokens.map((token, i) => {
        if (token.type === 'variable') {
            return <VariableToken key={i} token={token} />
        }

        const isInteractive = token.type === 'subflow'
        const navigateToSubflowByName = useFlowStore.getState().navigateToSubflowByName
        
        return (
          <span
            key={i}
            onClick={isInteractive ? (e) => {
                e.stopPropagation()
                navigateToSubflowByName(token.target!)
            } : undefined}
            title={token.type === 'subflow' ? `Jump to subflow: ${token.target}` : undefined}
            className={clsx(
              isInteractive && 'cursor-pointer',
              token.type === 'subflow' && 'inline-flex items-center gap-1 text-block-subflow font-semibold hover:underline decoration-block-subflow/30 underline-offset-2 transition-all',
              token.type === 'string' && 'text-block-string font-mono italic',
              token.type === 'text' && 'text-text-primary'
            )}
          >
            {token.value}
            {token.type === 'subflow' && <ExternalLink size={10} className="opacity-60" />}
          </span>
        )
      })}
    </div>
  )
}

function VariableToken({token}: {token: BlockToken}) {
    const document = useFlowStore(s => s.document)
    const selectedVariable = useUIStore(s => s.selectedVariable)
    const setSelectedVariable = useUIStore(s => s.setSelectedVariable)
    const setVariableLineage = useAnalysisStore(s => s.setVariableLineage)
    const setVariablePanelOpen = useUIStore(s => s.setVariablePanelOpen)

    const info = React.useMemo(() => {
        if (!document || !token.target) return null
        
        let decl = null
        for (const sf of document.subflows) {
            const d = sf.variables?.find?.(v => v.name === token.target)
            if (d) { decl = d; break }
        }

        let usageCount = 0
        const countUsage = (blocks: any[]) => {
            for (const b of blocks) {
                if (b.variables?.includes(token.target)) usageCount++
                if (b.children?.length) countUsage(b.children)
            }
        }
        for (const sf of document.subflows) countUsage(sf.blocks)

        return {decl, usageCount}
    }, [document, token.target])

    const isSelected = token.target === selectedVariable

    const handleClick = async (e: React.MouseEvent) => {
        if (!token.target) return
        e.stopPropagation()
        setSelectedVariable(token.target)
        try {
            const history = await analysisApi.getVariableLineage(token.target)
            if (history) {
                setVariableLineage(history as unknown as VariableHistory)
                setVariablePanelOpen(true)
            }
        } catch (err) {
            console.error('Failed to get variable lineage:', err)
        }
    }

    const tooltipContent = (
        <div className="flex flex-col gap-1.5 p-1 min-w-[140px]">
            <div className="flex items-center justify-between border-b border-white/10 pb-1 mb-1">
                <span className="font-bold text-brand-300">%{token.target}%</span>
                <VariableIcon size={12} className="text-text-tertiary" />
            </div>
            {info?.decl && (
                <>
                    <div className="flex justify-between gap-4">
                        <span className="text-text-tertiary">Type:</span>
                        <span className="text-text-secondary italic">{info.decl.type || 'Variant'}</span>
                    </div>
                    <div className="flex justify-between gap-4">
                        <span className="text-text-tertiary">Scope:</span>
                        <span className={clsx(
                            'font-medium',
                            info.decl.scope === 'flow' ? 'text-semantic-info' : 'text-semantic-warning'
                        )}>
                            {info.decl.scope}
                        </span>
                    </div>
                </>
            )}
            <div className="flex justify-between gap-4">
                <span className="text-text-tertiary">Usages:</span>
                <span className="text-text-primary font-mono">{info?.usageCount ?? 0}</span>
            </div>
            <div className="text-[10px] text-text-disabled mt-1 border-t border-white/5 pt-1 italic">
                Click to trace lineage
            </div>
        </div>
    )

    return (
        <Tooltip content={tooltipContent} delay={400}>
            <span
                onClick={handleClick}
                className={clsx(
                    'px-1.5 py-0.5 rounded-md font-mono text-[0.9em] border transition-all mx-0.5 leading-none inline-block cursor-pointer',
                    isSelected 
                        ? 'bg-yellow-400/30 border-yellow-500/50 text-yellow-900 dark:text-yellow-200 shadow-glow-sm' 
                        : 'bg-block-variable-bg text-block-variable border-block-variable/10 hover:bg-block-variable-bg/20 hover:border-block-variable/20'
                )}
            >
                {token.value.replace(/^%|%$/g, '')}
            </span>
        </Tooltip>
    )
}
