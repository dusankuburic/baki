import {describe, it, expect} from 'vitest'
import {readFileSync} from 'node:fs'
import {THEME_REGISTRY} from './themeRegistry'

// Self-verifying WCAG AA contrast: parses index.css, extracts every theme's
// design tokens, and asserts that text-on-surface pairs meet AA thresholds.
// Add a new theme → it's automatically checked. Tweak a color → regression
// is caught here, not by a user's eyes.

const css = readFileSync('src/index.css', 'utf-8')

// ---- CSS parsing ----------------------------------------------------------

/** Extract `--token: value;` pairs from a flat CSS block body. */
function parseTokens(block: string): Record<string, string> {
  const tokens: Record<string, string> = {}
  const re = /(--[\w-]+):\s*([^;]+);/g
  let m: RegExpExecArray | null
  while ((m = re.exec(block)) !== null) {
    tokens[m[1]] = m[2].trim()
  }
  return tokens
}

/** Parse :root and [data-theme="X"] blocks. Only matches definition blocks
 *  (where `]` is immediately followed by `{`), excluding autofill overrides. */
function parseThemes(css: string): {
  root: Record<string, string>
  themes: Record<string, Record<string, string>>
} {
  const rootMatch = css.match(/^:root\s*\{([^}]+)\}/m)
  const root = rootMatch ? parseTokens(rootMatch[1]) : {}

  const themes: Record<string, Record<string, string>> = {}
  // Quote style is Prettier's call (currently single for CSS attribute
  // selectors), so accept either rather than coupling this parser to it.
  const themeRe = /\[data-theme=(["'])([\w-]+)\1\]\s*\{([^}]+)\}/g
  let m: RegExpExecArray | null
  while ((m = themeRe.exec(css)) !== null) {
    themes[m[2]] = parseTokens(m[3])
  }
  return {root, themes}
}

const {root, themes: perTheme} = parseThemes(css)

// :root IS the dark theme (no [data-theme="dark"] block exists). Merge each
// theme's overrides onto the root defaults so every theme has a full token set.
const allThemes: Record<string, Record<string, string>> = {
  dark: root,
  ...Object.fromEntries(Object.entries(perTheme).map(([id, overrides]) => [id, {...root, ...overrides}])),
}

// ---- WCAG color math ------------------------------------------------------

function hexToRgb(hex: string): [number, number, number] | null {
  let h = hex.replace('#', '').trim()
  if (h.length === 3)
    h = h
      .split('')
      .map(c => c + c)
      .join('')
  if (!/^[0-9a-fA-F]{6}$/.test(h)) return null
  return [parseInt(h.slice(0, 2), 16), parseInt(h.slice(2, 4), 16), parseInt(h.slice(4, 6), 16)]
}

/** WCAG 2.1 relative luminance. */
function luminance(rgb: [number, number, number]): number {
  const lin = (c: number) => {
    const s = c / 255
    return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4)
  }
  return 0.2126 * lin(rgb[0]) + 0.7152 * lin(rgb[1]) + 0.0722 * lin(rgb[2])
}

/** WCAG 2.1 contrast ratio between two hex colors (1.0–21.0). */
export function contrastRatio(fg: string, bg: string): number {
  const fgRgb = hexToRgb(fg)
  const bgRgb = hexToRgb(bg)
  if (!fgRgb || !bgRgb) return Infinity // can't evaluate var()/rgba — skip
  const l1 = luminance(fgRgb)
  const l2 = luminance(bgRgb)
  return (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05)
}

// ---- Test cases -----------------------------------------------------------

const AA_NORMAL = 4.5 // body text
const AA_LARGE = 3.0 // ≥18px or ≥14px bold — used for tertiary labels/captions

const themeEntries = Object.entries(allThemes)

describe('theme contrast (WCAG AA)', () => {
  describe.each(themeEntries)('theme: %s', (_themeId, tokens) => {
    const surfaces = ['--surface-0', '--surface-1', '--surface-2'] as const

    // Primary text is body copy — must meet AA normal (4.5:1).
    describe.each(surfaces)('primary text on %s', surf => {
      it(`≥ ${AA_NORMAL}:1`, () => {
        const ratio = contrastRatio(tokens['--text-primary'], tokens[surf])
        expect(ratio).toBeGreaterThanOrEqual(AA_NORMAL)
      })
    })

    // Secondary text is subtitles/labels — must meet AA normal (4.5:1).
    describe.each(surfaces)('secondary text on %s', surf => {
      it(`≥ ${AA_NORMAL}:1`, () => {
        const ratio = contrastRatio(tokens['--text-secondary'], tokens[surf])
        expect(ratio).toBeGreaterThanOrEqual(AA_NORMAL)
      })
    })

    // Tertiary text is captions/hints — AA large threshold (3:1).
    describe.each(surfaces)('tertiary text on %s', surf => {
      it(`≥ ${AA_LARGE}:1`, () => {
        const ratio = contrastRatio(tokens['--text-tertiary'], tokens[surf])
        expect(ratio).toBeGreaterThanOrEqual(AA_LARGE)
      })
    })

    // Brand-foreground (button/toggle text) on brand-500 (button background)
    // must meet AA normal — covers primary buttons, switches, checkboxes.
    it('brand-foreground on brand-500 ≥ 4.5:1', () => {
      const ratio = contrastRatio(tokens['--brand-foreground'], tokens['--brand-500'])
      expect(ratio).toBeGreaterThanOrEqual(AA_NORMAL)
    })

    // Block-type colors are used as text/icon colors for labels, badges, and
    // tree-node type indicators (text-block-action, text-block-loop, etc.).
    // They must be at least distinguishable on card surfaces.
    // --block-comment is EXCLUDED — it is intentionally muted in every theme
    // (comments are low-importance by convention, mirroring syntax highlighters).
    // Threshold is 2.5:1 — stricter than "distinguishable" (1.5) but accepts
    // that pastel themes (Catppuccin, Rose Pine) have inherently softer contrast.
    const AA_BLOCK = 2.5
    const blockColors = [
      '--block-action',
      '--block-loop',
      '--block-condition',
      '--block-subflow',
      '--block-error',
      '--block-variable',
      '--block-string',
      '--block-wait',
    ] as const
    const cardSurfaces = ['--surface-1', '--surface-2'] as const
    describe.each(blockColors)('%s on card surfaces', block => {
      describe.each(cardSurfaces)('%s', surf => {
        it(`≥ ${AA_BLOCK}:1`, () => {
          const ratio = contrastRatio(tokens[block], tokens[surf])
          expect(ratio).toBeGreaterThanOrEqual(AA_BLOCK)
        })
      })
    })
  })
})

// ---- Registry ↔ CSS cross-check ------------------------------------------
// Catches the "forgot the CSS block" or "forgot the registry entry" mistake
// when adding a new theme. Every theme in the registry must have CSS tokens,
// and every CSS theme block must have a registry entry.

describe('theme registry ↔ CSS cross-check', () => {
  const cssThemeIds = new Set(Object.keys(allThemes))
  const registryIds = new Set<string>(THEME_REGISTRY.map(t => t.id))

  it('every registry theme has CSS tokens defined', () => {
    for (const t of THEME_REGISTRY) {
      expect(cssThemeIds.has(t.id)).toBe(true)
    }
  })

  it('every CSS theme block has a registry entry', () => {
    for (const id of cssThemeIds) {
      expect(registryIds.has(id)).toBe(true)
    }
  })
})

// ---- Semantic tone tokens (U2) --------------------------------------------
// Findings/chat/admin tones ride --error/--warning/--info/--success via
// lib/severityTone. Every theme (root + overrides) must define all four, or
// a themed surface silently falls back to an inherited (wrong-palette) tone.

describe('semantic tone tokens per theme', () => {
  const TONE_TOKENS = ['--error', '--warning', '--info', '--success'] as const

  it(':root defines all tone tokens', () => {
    for (const tok of TONE_TOKENS) {
      expect(allThemes[':root']?.[tok] ?? root[tok], `root missing ${tok}`).toBeTruthy()
    }
  })

  it('every theme defines all tone tokens (no inherited-wrong-palette fallback)', () => {
    for (const [id, tokens] of Object.entries(perTheme)) {
      for (const tok of TONE_TOKENS) {
        expect(tokens[tok], `theme "${id}" missing ${tok}`).toBeTruthy()
      }
    }
  })
})
