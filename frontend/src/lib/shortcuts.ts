export type ShortcutScope = 'global' | 'sidebar' | 'main' | 'inspector' | 'chat' | 'modal'

export interface Shortcut {
  id: string
  keys: string
  description: string
  category: 'File' | 'View' | 'Navigation' | 'Edit' | 'AI' | 'Analysis' | 'Window' | 'Help'
  scope: ShortcutScope
  preventDefault?: boolean
  allowInInputs?: boolean
}

export const shortcuts: Shortcut[] = [
  {id: 'file.open', keys: 'mod+o', description: 'Open flow file', category: 'File', scope: 'global'},
  {id: 'file.export.pdf', keys: 'mod+e', description: 'Export findings as PDF', category: 'File', scope: 'global'},
  {id: 'file.export.md', keys: 'mod+shift+e', description: 'Export as Markdown', category: 'File', scope: 'global'},
  {id: 'file.close.tab', keys: 'mod+w', description: 'Close active tab', category: 'File', scope: 'global'},
  {id: 'file.close.others', keys: 'mod+alt+w', description: 'Close other tabs in group', category: 'File', scope: 'global'},
  {id: 'file.close.all', keys: 'mod+shift+w', description: 'Close all tabs in group', category: 'File', scope: 'global'},

  {id: 'view.toggle.sidebar', keys: 'mod+b', description: 'Toggle sidebar', category: 'View', scope: 'global'},
  {id: 'view.toggle.inspector', keys: 'mod+i', description: 'Toggle inspector', category: 'View', scope: 'global'},
  {id: 'view.toggle.mode', keys: 'mod+g', description: 'Toggle block / graph view', category: 'View', scope: 'global'},
  {id: 'view.map', keys: 'mod+m', description: 'Show subflow map', category: 'View', scope: 'global'},
  {id: 'view.local-map', keys: 'mod+shift+m', description: 'Show local subflow map', category: 'View', scope: 'global'},
  {id: 'view.tab.details', keys: 'mod+1', description: 'Inspector → Details', category: 'View', scope: 'global'},
  {id: 'view.tab.ai', keys: 'mod+2', description: 'Inspector → AI', category: 'View', scope: 'global'},
  {id: 'view.tab.findings', keys: 'mod+3', description: 'Inspector → Findings', category: 'View', scope: 'global'},
  {id: 'view.fullscreen', keys: 'f11', description: 'Toggle fullscreen', category: 'View', scope: 'global'},
  {id: 'view.theme.toggle', keys: 'mod+shift+t', description: 'Toggle theme', category: 'View', scope: 'global'},
  {id: 'view.split.toggle', keys: 'mod+\\', description: 'Split editor right', category: 'View', scope: 'global'},
  {id: 'view.group.1', keys: 'alt+1', description: 'Focus group 1', category: 'View', scope: 'global'},
  {id: 'view.group.2', keys: 'alt+2', description: 'Focus group 2', category: 'View', scope: 'global'},
  {id: 'view.group.3', keys: 'alt+3', description: 'Focus group 3', category: 'View', scope: 'global'},
  {id: 'view.group.4', keys: 'alt+4', description: 'Focus group 4', category: 'View', scope: 'global'},
  {id: 'view.move.group.right', keys: 'mod+alt+right', description: 'Move tab to next group', category: 'View', scope: 'global'},
  {id: 'view.move.group.left', keys: 'mod+alt+left', description: 'Move tab to previous group', category: 'View', scope: 'global'},

  {id: 'nav.search', keys: 'mod+f', description: 'Focus sidebar search', category: 'Navigation', scope: 'global'},
  {id: 'nav.search.global', keys: 'mod+shift+f', description: 'Global search overlay', category: 'Navigation', scope: 'global'},
  {id: 'nav.search.quick', keys: '/', description: 'Quick search', category: 'Navigation', scope: 'global'},
  {id: 'nav.palette', keys: 'mod+k', description: 'Command palette', category: 'Navigation', scope: 'global'},
  {id: 'nav.settings', keys: 'mod+,', description: 'Open settings', category: 'Navigation', scope: 'global'},
  {id: 'nav.next.block', keys: 'j', description: 'Next block', category: 'Navigation', scope: 'main'},
  {id: 'nav.prev.block', keys: 'k', description: 'Previous block', category: 'Navigation', scope: 'main'},
  {id: 'nav.next.finding', keys: 'n', description: 'Next finding', category: 'Navigation', scope: 'main'},
  {id: 'nav.prev.finding', keys: 'shift+n', description: 'Previous finding', category: 'Navigation', scope: 'main'},
  {id: 'nav.parent', keys: 'mod+up', description: 'Parent block', category: 'Navigation', scope: 'main'},
  {id: 'nav.first.child', keys: 'mod+down', description: 'First child', category: 'Navigation', scope: 'main'},
  {id: 'nav.up.subflow', keys: 'mod+shift+up', description: 'Up to parent subflow', category: 'Navigation', scope: 'global'},
  {id: 'nav.drill.subflow', keys: 'enter', description: 'Drill into subflow', category: 'Navigation', scope: 'main'},

  {id: 'tree.expand', keys: 'right', description: 'Expand node', category: 'Navigation', scope: 'sidebar'},
  {id: 'tree.collapse', keys: 'left', description: 'Collapse node', category: 'Navigation', scope: 'sidebar'},
  {id: 'tree.expand.all', keys: 'mod+shift+right', description: 'Expand all', category: 'Navigation', scope: 'sidebar'},
  {id: 'tree.collapse.all', keys: 'mod+shift+left', description: 'Collapse all', category: 'Navigation', scope: 'sidebar'},

  {id: 'edit.copy.name', keys: 'mod+c', description: 'Copy block name', category: 'Edit', scope: 'main'},
  {id: 'edit.copy.path', keys: 'mod+shift+c', description: 'Copy block path', category: 'Edit', scope: 'main'},
  {id: 'edit.clear.selection', keys: 'escape', description: 'Clear selection', category: 'Edit', scope: 'main', preventDefault: false},

  {id: 'ai.send', keys: 'mod+enter', description: 'Send AI message', category: 'AI', scope: 'chat', allowInInputs: true},
  {id: 'ai.clear.chat', keys: 'mod+l', description: 'Clear chat', category: 'AI', scope: 'chat'},
  {id: 'ai.ask.selection', keys: 'mod+shift+a', description: 'Ask AI about selection', category: 'AI', scope: 'global'},
  {id: 'ai.next.suggestion', keys: 'tab', description: 'Next suggested prompt', category: 'AI', scope: 'chat'},
  {id: 'ai.cancel.stream', keys: 'escape', description: 'Cancel AI response', category: 'AI', scope: 'chat', preventDefault: false},

  {id: 'analysis.run', keys: 'mod+shift+r', description: 'Run analysis', category: 'Analysis', scope: 'global', allowInInputs: false},
  {id: 'analysis.export.html', keys: 'mod+shift+h', description: 'Export findings as HTML', category: 'Analysis', scope: 'global'},
  // mod+alt+e: mod+shift+e belongs to file.export.md (declared in App.tsx).
  {id: 'analysis.export.csv', keys: 'mod+alt+e', description: 'Export findings as CSV', category: 'Analysis', scope: 'global'},
  {id: 'analysis.filter.errors', keys: 'mod+shift+1', description: 'Filter errors only', category: 'Analysis', scope: 'global'},
  {id: 'analysis.filter.warnings', keys: 'mod+shift+2', description: 'Filter warnings only', category: 'Analysis', scope: 'global'},
  {id: 'analysis.filter.info', keys: 'mod+shift+3', description: 'Filter info only', category: 'Analysis', scope: 'global'},
  {id: 'analysis.filter.all', keys: 'mod+shift+0', description: 'Show all findings', category: 'Analysis', scope: 'global'},
  {id: 'analysis.tab.metrics', keys: 'mod+4', description: 'Inspector → Metrics', category: 'Analysis', scope: 'global'},

  {id: 'window.reload', keys: 'mod+r', description: 'Reload app', category: 'Window', scope: 'global'},
  {id: 'window.devtools', keys: 'mod+alt+i', description: 'Open dev tools', category: 'Window', scope: 'global'},
  {id: 'window.quit', keys: 'mod+q', description: 'Quit app', category: 'Window', scope: 'global'},

  {id: 'help.shortcuts', keys: '?', description: 'Show shortcuts help', category: 'Help', scope: 'global'},
]

const isMac = typeof navigator !== 'undefined' && /Mac|iPod|iPhone|iPad/.test(navigator.userAgent)

export function matchesShortcut(e: KeyboardEvent, keys: string): boolean {
  const parts = keys.toLowerCase().split('+')
  const expected = {
    mod: parts.includes('mod'),
    cmd: parts.includes('cmd'),
    ctrl: parts.includes('ctrl'),
    shift: parts.includes('shift'),
    alt: parts.includes('alt'),
    key: parts.filter(p => !['mod', 'cmd', 'ctrl', 'shift', 'alt'].includes(p))[0],
  }

  const modPressed = isMac ? e.metaKey : e.ctrlKey
  if (expected.mod && !modPressed) return false
  if (!expected.mod && modPressed && expected.key) return false

  if (expected.cmd && !e.metaKey) return false
  if (expected.ctrl && !e.ctrlKey) return false
  if (expected.shift !== e.shiftKey) return false
  if (expected.alt !== e.altKey) return false

  const eventKey = normalizeKey(e.key)
  return eventKey === expected.key
}

function normalizeKey(key: string): string {
  const map: Record<string, string> = {
    ArrowUp: 'up', ArrowDown: 'down', ArrowLeft: 'left', ArrowRight: 'right',
    ' ': 'space', Enter: 'enter', Escape: 'escape', Tab: 'tab',
    Backspace: 'backspace', Delete: 'delete',
  }
  return (map[key] ?? key).toLowerCase()
}

function formatKeyDisplay(k: string): string {
  switch (k) {
    case 'mod': return isMac ? '⌘' : 'Ctrl'
    case 'shift': return isMac ? '⇧' : 'Shift'
    case 'alt': return isMac ? '⌥' : 'Alt'
    case 'ctrl': return isMac ? '⌃' : 'Ctrl'
    case 'escape': return 'Esc'
    case 'enter': return isMac ? '↵' : 'Enter'
    case 'backspace': return isMac ? '⌫' : 'Backspace'
    case 'delete': return isMac ? '⌦' : 'Del'
    case 'tab': return isMac ? '⇥' : 'Tab'
    case 'space': return 'Space'
    default: return k.charAt(0).toUpperCase() + k.slice(1)
  }
}

export function formatShortcutKeys(keys: string): string {
  return keys
    .split('+')
    .map(k => formatKeyDisplay(k))
    .join(isMac ? '' : '+')
}

export function formatShortcutParts(keys: string): {key: string; display: string}[] {
  return keys.split('+').map(k => ({key: k, display: formatKeyDisplay(k)}))
}

export function checkCollisions(): Map<string, string[]> {
  const keyMap = new Map<string, string[]>()
  for (const s of shortcuts) {
    const normalized = s.keys.toLowerCase()
    const existing = keyMap.get(normalized) ?? []
    existing.push(s.id)
    keyMap.set(normalized, existing)
  }
  const collisions = new Map<string, string[]>()
  for (const [key, ids] of keyMap) {
    if (ids.length > 1) collisions.set(key, ids)
  }
  return collisions
}
