import {useMemo} from 'react'
import {useUIStore} from '@/stores/uiStore'

/**
 * Colours the graph views need as values rather than as CSS: SVG arrow markers, edge strokes and
 * the minimap's node dots, which React Flow takes as strings.
 *
 * Everything else about a node or edge is styled by class in `index.css`, so only these few are
 * resolved from the theme's custom properties here.
 */
export interface FlowColors {
  surface0: string
  surface1: string
  edge: string
  edgeMuted: string
  brand: string
  error: string
  warning: string
  info: string
  block: Record<string, string>
}

const BLOCK_VARS: Record<string, string> = {
  ACTION: '--block-action',
  LOOP: '--block-loop',
  CONDITION: '--block-condition',
  SUBFLOW: '--block-subflow',
  ERROR_HANDLER: '--block-error',
  COMMENT: '--block-comment',
  VARIABLE: '--block-variable',
  WAIT: '--block-wait',
  BLOCK: '--block-action',
  SWITCH: '--block-condition',
  CASE: '--block-condition',
  DEFAULT: '--block-condition',
}

export function resolveFlowColors(): FlowColors {
  const style = typeof window === 'undefined' ? null : getComputedStyle(document.documentElement)
  const read = (name: string, fallback: string) => style?.getPropertyValue(name).trim() || fallback

  const block: Record<string, string> = {}
  for (const [type, variable] of Object.entries(BLOCK_VARS)) block[type] = read(variable, '#6b7280')

  return {
    surface0: read('--surface-0', '#ffffff'),
    surface1: read('--surface-1', '#f8fafc'),
    edge: read('--border-strong', '#94a3b8'),
    edgeMuted: read('--border-default', '#cbd5e1'),
    brand: read('--brand-500', '#3b82f6'),
    error: read('--error', '#ef4444'),
    warning: read('--warning', '#f59e0b'),
    info: read('--info', '#3b82f6'),
    block,
  }
}

/** The theme's colours, re-read whenever the resolved theme changes. */
export function useFlowColors(): FlowColors {
  const resolvedTheme = useUIStore(s => s.resolvedTheme)
  // The custom properties themselves change with the theme, so the theme is the cache key.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  return useMemo(() => resolveFlowColors(), [resolvedTheme])
}

export function blockColor(colors: FlowColors, type: string | undefined): string {
  return (type && colors.block[type]) || colors.block.COMMENT || '#6b7280'
}
