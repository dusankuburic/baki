import React, {useMemo} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import type {BlockToken, Block, FlowDocument, VariableDecl} from '@/types'
import clsx from 'clsx'

import {analysisApi} from '@/api'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useUIStore} from '@/stores/uiStore'
import {logger} from '@/lib/logger'

import {ExternalLink, Variable as VariableIcon, Hash} from 'lucide-react'
import {Tooltip} from '@/components/shared'

interface TokenRendererProps {
  tokens: BlockToken[]
}

type VarInfo = {decl: VariableDecl | null; usageCount: number}

// Shared index: built once per document change via useMemo in TokenRenderer,
// consumed by all VariableToken instances via context — avoids O(tokens ×
// blocks) repeated full-tree walks.
const VariableIndexContext = React.createContext<Map<string, VarInfo>>(new Map())

function buildVariableIndex(doc: FlowDocument | null): Map<string, VarInfo> {
  const index = new Map<string, VarInfo>()
  if (!doc) return index
  // Collect declarations
  for (const sf of doc.subflows) {
    if (sf.variables) {
      for (const v of sf.variables) {
        if (!index.has(v.name)) index.set(v.name, {decl: v, usageCount: 0})
      }
    }
  }
  // Count usages in a single tree walk (O(blocks), not O(vars × blocks))
  const countInBlocks = (blocks: Block[]) => {
    for (const b of blocks) {
      if (b.variables) {
        for (const v of b.variables) {
          const entry = index.get(v)
          if (entry) entry.usageCount++
        }
      }
      if (b.children?.length) countInBlocks(b.children)
    }
  }
  for (const sf of doc.subflows) countInBlocks(sf.blocks)
  return index
}

export default function TokenRenderer({tokens}: TokenRendererProps) {
  const document = useFlowStore(s => s.document)
  const index = useMemo(() => buildVariableIndex(document), [document])

  return (
    <VariableIndexContext.Provider value={index}>
      <div className="flex flex-wrap items-center">
        {tokens.map((token, i) => {
          if (token.type === 'variable') {
            return <VariableToken key={i} token={token} />
          }

          if (token.type === 'label') {
            return <LabelToken key={i} token={token} />
          }

          const isInteractive = token.type === 'subflow'
          const navigateToSubflowByName = useFlowStore.getState().navigateToSubflowByName

          return (
            <span
              key={i}
              onClick={
                isInteractive
                  ? e => {
                      e.stopPropagation()
                      if (token.target) navigateToSubflowByName(token.target)
                    }
                  : undefined
              }
              title={token.type === 'subflow' ? `Jump to subflow: ${token.target}` : undefined}
              className={clsx(
                isInteractive && 'cursor-pointer',
                token.type === 'subflow' &&
                  'inline-flex items-center gap-1 text-block-subflow font-semibold hover:underline decoration-block-subflow/30 underline-offset-2 transition-all duration-fast',
                token.type === 'string' && 'text-block-string font-mono italic',
                token.type === 'text' && 'text-text-primary whitespace-pre-wrap',
              )}
            >
              {token.value}
              {token.type === 'subflow' && <ExternalLink size={10} className="opacity-60" />}
            </span>
          )
        })}
      </div>
    </VariableIndexContext.Provider>
  )
}

function LabelToken({token}: {token: BlockToken}) {
  const navigateToLabelByName = useFlowStore(s => s.navigateToLabelByName)
  const isGoto = !!token.target

  const handleClick = (e: React.MouseEvent) => {
    if (!isGoto || !token.target) return
    e.stopPropagation()
    navigateToLabelByName(token.target)
  }

  return (
    <span
      onClick={handleClick}
      className={clsx(
        'inline-flex items-center gap-0.5 px-1.5 py-0.5 rounded-md font-mono text-[0.9em] border mx-0.5 leading-none bg-purple-500/10 text-purple-400 border-purple-500/20 transition-colors',
        isGoto && 'cursor-pointer hover:bg-purple-500/20 hover:border-purple-500/40',
      )}
    >
      <Hash size={10} className="opacity-70" />
      {token.value}
    </span>
  )
}

function VariableToken({token}: {token: BlockToken}) {
  const index = React.useContext(VariableIndexContext)
  const selectedVariable = useUIStore(s => s.selectedVariable)
  const setSelectedVariable = useUIStore(s => s.setSelectedVariable)
  const setVariableLineage = useAnalysisStore(s => s.setVariableLineage)
  const setVariablePanelOpen = useUIStore(s => s.setVariablePanelOpen)

  const info = token.target ? (index.get(token.target) ?? null) : null

  const isSelected = token.target === selectedVariable

  const handleClick = async (e: React.MouseEvent) => {
    if (!token.target) return
    e.stopPropagation()
    setSelectedVariable(token.target)
    try {
      const history = await analysisApi.getVariableLineage(token.target)
      if (history) {
        setVariableLineage(history)
        setVariablePanelOpen(true)
      }
    } catch (err) {
      logger.warn('Failed to get variable lineage:', err)
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
            <span
              className={clsx(
                'font-medium',
                info.decl.scope === 'flow' ? 'text-semantic-info' : 'text-semantic-warning',
              )}
            >
              {info.decl.scope}
            </span>
          </div>
        </>
      )}
      <div className="flex justify-between gap-4">
        <span className="text-text-tertiary">Usages:</span>
        <span className="text-text-primary font-mono">{info?.usageCount ?? 0}</span>
      </div>
      <div className="text-2xs text-text-disabled mt-1 border-t border-white/5 pt-1 italic">Click to trace lineage</div>
    </div>
  )

  return (
    <Tooltip content={tooltipContent} delay={400}>
      <span
        onClick={handleClick}
        className={clsx(
          'px-1.5 py-0.5 rounded-md font-mono text-[0.9em] border transition-all duration-fast mx-0.5 leading-none inline-block cursor-pointer',
          isSelected
            ? 'bg-block-string/20 border-block-string/50 text-text-primary shadow-glow-sm'
            : 'bg-block-variable-bg text-block-variable border-block-variable/10 hover:bg-block-variable-bg/20 hover:border-block-variable/20',
        )}
      >
        {token.value.replace(/^%|%$/g, '')}
      </span>
    </Tooltip>
  )
}
