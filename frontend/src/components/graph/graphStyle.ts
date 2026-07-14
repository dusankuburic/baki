import cytoscape from 'cytoscape'

export interface GraphTokenColors {
  surface2: string
  surface3: string
  borderDefault: string
  borderStrong: string
  textPrimary: string
  textSecondary: string
  brand500: string
  error: string
  warning: string
  info: string
  blockAction: string
  blockLoop: string
  blockCondition: string
  blockSubflow: string
  blockError: string
  blockComment: string
  blockVariable: string
  blockWait: string
}

export function resolveGraphTokens(): GraphTokenColors {
  const s = getComputedStyle(document.documentElement)
  const get = (name: string) => s.getPropertyValue(name).trim()
  return {
    surface2: get('--surface-2'),
    surface3: get('--surface-3') || '#25252b',
    borderDefault: get('--border-default'),
    borderStrong: get('--border-strong'),
    textPrimary: get('--text-primary'),
    textSecondary: get('--text-secondary'),
    brand500: get('--brand-500'),
    error: get('--error') || '#ef4444',
    warning: get('--warning') || '#f59e0b',
    info: get('--info') || '#3b82f6',
    blockAction: get('--block-action'),
    blockLoop: get('--block-loop'),
    blockCondition: get('--block-condition'),
    blockSubflow: get('--block-subflow'),
    blockError: get('--block-error'),
    blockComment: get('--block-comment'),
    blockVariable: get('--block-variable'),
    blockWait: get('--block-wait'),
  }
}

const typeColorMap: Record<string, keyof GraphTokenColors> = {
  ACTION: 'blockAction',
  LOOP: 'blockLoop',
  CONDITION: 'blockCondition',
  SUBFLOW: 'blockSubflow',
  ERROR_HANDLER: 'blockError',
  COMMENT: 'blockComment',
  VARIABLE: 'blockVariable',
  WAIT: 'blockWait',
  BLOCK: 'blockAction',
  SWITCH: 'blockCondition',
  CASE: 'blockCondition',
  DEFAULT: 'blockCondition',
}

export function buildGraphStyle(t: GraphTokenColors): cytoscape.StylesheetJson {
  const typeStyles: cytoscape.StylesheetJson = Object.entries(typeColorMap).map(([type, key]) => ({
    selector: `node[type="${type}"]`,
    style: {
      'border-color': t[key],
      'border-width': 2,
    },
  }))

  return [
    {
      selector: 'node',
      style: {
        'background-color': t.surface2,
        'border-color': t.borderDefault,
        'border-width': 1,
        shape: 'round-rectangle',
        width: 210,
        height: 88,
        padding: 12 as unknown as string,
        // fullLabel contains "TypeLabel\nStrippedName" (two lines)
        label: 'data(fullLabel)',
        color: t.textPrimary,
        'font-size': 12,
        'font-weight': 400,
        'text-valign': 'center',
        'text-halign': 'center',
        'text-wrap': 'wrap',
        'text-overflow-wrap': 'anywhere',
        'text-max-width': 180 as unknown as string,
      },
    },
    ...typeStyles,
    {
      selector: 'node:selected',
      style: {
        'border-color': t.brand500,
        'border-width': 2,
        'overlay-color': t.brand500,
        'overlay-opacity': 0.1,
      },
    },
    {
      selector: 'edge',
      style: {
        'curve-style': 'bezier',
        'target-arrow-shape': 'triangle',
        'line-color': t.borderStrong,
        'target-arrow-color': t.borderStrong,
        width: 2,
      },
    },
    {
      selector: 'edge.highlighted',
      style: {
        'line-color': t.brand500,
        'target-arrow-color': t.brand500,
        width: 3,
      },
    },
    {
      selector: 'node.variable-dimmed',
      style: {
        opacity: 0.3,
      },
    },
    {
      selector: 'edge.variable-dimmed',
      style: {
        opacity: 0.1,
      },
    },
    {
      selector: 'node.variable-highlighted',
      style: {
        'border-color': t.warning,
        'border-width': 3,
        'background-color': t.surface2,
      },
    },
    {
      selector: 'node.finding-error',
      style: {
        'border-color': t.error,
        'border-width': 3,
      },
    },
    {
      selector: 'node.finding-warning',
      style: {
        'border-color': t.warning,
        'border-width': 3,
      },
    },
    {
      selector: 'node.finding-info',
      style: {
        'border-color': t.info,
        'border-width': 3,
      },
    },
  ]
}
