import {
    Settings2, Repeat, GitBranch, Layers, ShieldAlert,
    MessageSquare, Variable, Clock, BoxSelect, ArrowLeftRight, HelpCircle,
    type LucideIcon,
} from 'lucide-react'

interface BlockConfig {
    icon?: LucideIcon
    color: string
    bg?: string
    label: string
}

export const blockConfig: Record<string, BlockConfig> = {
    ACTION:        {icon: Settings2,      color: 'var(--block-action)',     bg: 'var(--block-action-bg)',     label: 'Action'      },
    LOOP:          {icon: Repeat,         color: 'var(--block-loop)',       bg: 'var(--block-loop-bg)',       label: 'Loop'        },
    CONDITION:     {icon: GitBranch,      color: 'var(--block-condition)',  bg: 'var(--block-condition-bg)', label: 'If'          },
    SUBFLOW:       {icon: Layers,         color: 'var(--block-subflow)',    bg: 'var(--block-subflow-bg)',    label: 'Subflow'     },
    ERROR_HANDLER: {icon: ShieldAlert,    color: 'var(--block-error)',      bg: 'var(--block-error-bg)',      label: 'On Error'    },
    COMMENT:       {icon: MessageSquare,  color: 'var(--block-comment)',    bg: 'var(--block-comment-bg)',    label: 'Comment'     },
    VARIABLE:      {icon: Variable,       color: 'var(--block-variable)',   bg: 'var(--block-variable-bg)',  label: 'Set Variable'},
    WAIT:          {icon: Clock,          color: 'var(--block-wait)',       bg: 'var(--block-wait-bg)',       label: 'Wait'        },
    BLOCK:         {icon: BoxSelect,      color: 'var(--block-action)',     bg: 'var(--block-action-bg)',     label: 'Block'       },
    SWITCH:        {icon: ArrowLeftRight, color: 'var(--block-condition)',  bg: 'var(--block-condition-bg)', label: 'Switch'      },
    ELSE:          {                      color: 'var(--block-condition)',                                   label: 'Else'        },
    CASE:          {                      color: 'var(--block-condition)',                                   label: 'Case'        },
    DEFAULT:       {                      color: 'var(--block-condition)',                                   label: 'Default'     },
    UNKNOWN:       {icon: HelpCircle,     color: 'var(--text-tertiary)',    bg: 'var(--surface-2)',           label: 'Unknown'     },
}

export function getBlockIcon(type: string): LucideIcon {
    return blockConfig[type]?.icon ?? HelpCircle
}

export function getBlockColor(type: string): string {
    return blockConfig[type]?.color ?? 'var(--text-tertiary)'
}

export function getBlockBg(type: string): string {
    return blockConfig[type]?.bg ?? 'var(--surface-2)'
}

// ---------- label helpers shared between block view and graph view ----------

/** Returns the human-readable type badge label for a block. */
export function resolveTypeLabel(type: string, name: string, rawType = ''): string {
    if (rawType === 'GOTO') return 'Goto'
    if (rawType === 'LABEL') return 'Label'
    if (rawType === 'EXIT_LOOP') return 'Exit Loop'
    if (rawType === 'NEXT_LOOP') return 'Next Loop'
    if (type === 'LOOP') {
        const n = name.toUpperCase()
        if (n.startsWith('LOOP FOREACH')) return 'ForEach Loop'
        if (n.startsWith('LOOP WHILE')) return 'While Loop'
        return 'Range Loop'
    }
    // Variables.* list/math operations are reclassified as ACTION by the parser.
    // Use the humanized action name as the badge label (e.g. "Create New List",
    // "Add Item To List") so each block is self-describing without showing
    // the raw module prefix or the generic "Action" catch-all.
    if (type === 'ACTION' && rawType.startsWith('Variables.')) {
        return name
    }
    return blockConfig[type]?.label ?? type
}

/** Returns true for loop-control flow statements (continue / break). */
export function isLoopControl(rawType: string): boolean {
    return rawType === 'EXIT_LOOP' || rawType === 'NEXT_LOOP'
}

/** Strips redundant structural keywords from a block name so only the payload is visible. */
export function stripBlockKeywords(type: string, name: string): string {
    // Aggressively strip common keywords that might be in the Name field from the parser
    let stripped = name.replace(/^\s*(SWITCH|CASE|IF|ELSE|LOOP|SET|CALL|WAIT|SELECT)\s+/i, '').trim()

    if (type === 'CONDITION') {
        stripped = stripped.replace(/\s+THEN\s*$/i, '').trim()
    } else if (type === 'LOOP') {
        stripped = stripped
            .replace(/^\s*FOREACH\s+/i, '')
            .replace(/^\s*WHILE\s+/i, '')
            .trim()
    }

    // Remove surrounding % if it's a simple variable reference
    return stripped.replace(/^%|%$/g, '')
}
