import {describe, it, expect, vi, afterEach} from 'vitest'
import {allShortcuts, matchesShortcut, formatShortcutParts} from './shortcuts'

function keyEvent(init: Partial<KeyboardEvent> & {key: string}): KeyboardEvent {
  return {
    key: init.key,
    metaKey: init.metaKey ?? false,
    ctrlKey: init.ctrlKey ?? false,
    shiftKey: init.shiftKey ?? false,
    altKey: init.altKey ?? false,
  } as KeyboardEvent
}

describe('matchesShortcut (non-mac / ctrl-based)', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('matches a mod+letter combo via ctrlKey on non-mac', () => {
    expect(matchesShortcut(keyEvent({key: 'b', ctrlKey: true}), 'mod+b')).toBe(true)
  })

  it('does not match when the modifier is missing', () => {
    expect(matchesShortcut(keyEvent({key: 'b'}), 'mod+b')).toBe(false)
  })

  it('does not match when shift state differs', () => {
    expect(matchesShortcut(keyEvent({key: 'e', ctrlKey: true}), 'mod+shift+e')).toBe(false)
    expect(matchesShortcut(keyEvent({key: 'e', ctrlKey: true, shiftKey: true}), 'mod+shift+e')).toBe(true)
  })

  it('does not match when a plain key is pressed with an unexpected mod held', () => {
    expect(matchesShortcut(keyEvent({key: 'j', ctrlKey: true}), 'j')).toBe(false)
    expect(matchesShortcut(keyEvent({key: 'j'}), 'j')).toBe(true)
  })

  it('normalizes special key names like ArrowUp/Escape before comparing', () => {
    expect(matchesShortcut(keyEvent({key: 'Escape'}), 'escape')).toBe(true)
    expect(matchesShortcut(keyEvent({key: 'ArrowUp', ctrlKey: true}), 'mod+up')).toBe(true)
  })

  it('is case-insensitive on the letter key', () => {
    expect(matchesShortcut(keyEvent({key: 'K', ctrlKey: true}), 'mod+k')).toBe(true)
  })
})

describe('formatShortcutParts', () => {
  it('splits a combo into individual key/display parts', () => {
    expect(formatShortcutParts('mod+shift+e')).toEqual([
      {key: 'mod', display: 'Ctrl'},
      {key: 'shift', display: 'Shift'},
      {key: 'e', display: 'E'},
    ])
  })
})

describe('registry', () => {
  // A combo may repeat across scopes that never listen together (escape clears
  // the canvas selection in 'main' and stops a reply in 'chat'). Within one
  // scope, or against a 'global' binding that is live everywhere, useKeyboard
  // runs whichever id comes first and the other entry is dead.
  it('binds each key combo at most once per scope, counting global against every scope', () => {
    const conflicts: string[] = []
    allShortcuts.forEach((a, i) => {
      for (const b of allShortcuts.slice(i + 1)) {
        if (a.keys.toLowerCase() !== b.keys.toLowerCase()) continue
        if (a.scope === b.scope || a.scope === 'global' || b.scope === 'global') {
          conflicts.push(`${a.keys}: ${a.id} / ${b.id}`)
        }
      }
    })
    expect(conflicts).toEqual([])
  })

  it('has unique ids', () => {
    const ids = allShortcuts.map(s => s.id)
    expect(new Set(ids).size).toBe(ids.length)
  })
})
