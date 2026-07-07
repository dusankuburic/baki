import {describe, it, expect, vi, afterEach} from 'vitest'
import {shortcuts, matchesShortcut, formatShortcutKeys, formatShortcutParts, checkCollisions} from './shortcuts'

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

describe('formatShortcutKeys', () => {
  it('renders a combo as Ctrl+Letter on non-mac', () => {
    expect(formatShortcutKeys('mod+k')).toBe('Ctrl+K')
  })

  it('renders a single special key', () => {
    expect(formatShortcutKeys('escape')).toBe('Esc')
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

describe('checkCollisions', () => {
  it('flags "escape" as shared, since checkCollisions is scope-unaware even though main/chat scopes never fire together', () => {
    // Every other key combo in the real registry is unique; 'escape' is
    // intentionally reused across scopes (edit.clear.selection / ai.cancel.stream).
    const collisions = checkCollisions()
    expect([...collisions.keys()]).toEqual(['escape'])
    expect(collisions.get('escape')).toEqual(['edit.clear.selection', 'ai.cancel.stream'])
  })

  it('detects colliding key combos when duplicated', () => {
    const original = [...shortcuts]
    shortcuts.push({id: 'dup.one', keys: 'mod+zzz', description: 'x', category: 'Help', scope: 'global'})
    shortcuts.push({id: 'dup.two', keys: 'mod+zzz', description: 'y', category: 'Help', scope: 'global'})
    try {
      const collisions = checkCollisions()
      expect(collisions.get('mod+zzz')).toEqual(['dup.one', 'dup.two'])
    } finally {
      shortcuts.length = 0
      shortcuts.push(...original)
    }
  })
})
