import {describe, it, expect} from 'vitest'
import {readFileSync} from 'node:fs'
import {THEME_REGISTRY} from './themeRegistry'
import {BADGE_TONES} from '@/components/shared/Badge'

const css = readFileSync('src/index.css', 'utf-8')

function parseTokens(block: string): Record<string, string> {
  const tokens: Record<string, string> = {}
  const re = /(--[\w-]+):\s*([^;]+);/g
  let m: RegExpExecArray | null
  while ((m = re.exec(block)) !== null) {
    tokens[m[1]!] = m[2]!.trim()
  }
  return tokens
}

function parseThemes(css: string): {
  root: Record<string, string>
  themes: Record<string, Record<string, string>>
} {
  const rootMatch = css.match(/^:root\s*\{([^}]+)\}/m)
  const root = rootMatch ? parseTokens(rootMatch[1]!) : {}

  const themes: Record<string, Record<string, string>> = {}
  const themeRe = /\[data-theme=(["'])([\w-]+)\1\]\s*\{([^}]+)\}/g
  let m: RegExpExecArray | null
  while ((m = themeRe.exec(css)) !== null) {
    themes[m[2]!] = parseTokens(m[3]!)
  }
  return {root, themes}
}

const {root, themes: perTheme} = parseThemes(css)

const allThemes: Record<string, Record<string, string>> = {
  dark: root,
  ...Object.fromEntries(Object.entries(perTheme).map(([id, overrides]) => [id, {...root, ...overrides}])),
}

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

function luminance(rgb: [number, number, number]): number {
  const lin = (c: number) => {
    const s = c / 255
    return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4)
  }
  return 0.2126 * lin(rgb[0]) + 0.7152 * lin(rgb[1]) + 0.0722 * lin(rgb[2])
}

export function contrastRatio(fg: string, bg: string): number {
  const fgRgb = hexToRgb(fg)
  const bgRgb = hexToRgb(bg)
  if (!fgRgb || !bgRgb) return Infinity
  const l1 = luminance(fgRgb)
  const l2 = luminance(bgRgb)
  return (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05)
}

const AA_NORMAL = 4.5
const AA_LARGE = 3.0

const themeEntries = Object.entries(allThemes)

describe('theme contrast (WCAG AA)', () => {
  describe.each(themeEntries)('theme: %s', (_themeId, tokens) => {
    const surfaces = ['--surface-0', '--surface-1', '--surface-2'] as const

    describe.each(surfaces)('primary text on %s', surf => {
      it(`≥ ${AA_NORMAL}:1`, () => {
        const ratio = contrastRatio(tokens['--text-primary']!, tokens[surf]!)
        expect(ratio).toBeGreaterThanOrEqual(AA_NORMAL)
      })
    })

    describe.each(surfaces)('secondary text on %s', surf => {
      it(`≥ ${AA_NORMAL}:1`, () => {
        const ratio = contrastRatio(tokens['--text-secondary']!, tokens[surf]!)
        expect(ratio).toBeGreaterThanOrEqual(AA_NORMAL)
      })
    })

    describe.each(surfaces)('tertiary text on %s', surf => {
      it(`≥ ${AA_LARGE}:1`, () => {
        const ratio = contrastRatio(tokens['--text-tertiary']!, tokens[surf]!)
        expect(ratio).toBeGreaterThanOrEqual(AA_LARGE)
      })
    })

    it('brand-foreground on brand-500 ≥ 4.5:1', () => {
      const ratio = contrastRatio(tokens['--brand-foreground']!, tokens['--brand-500']!)
      expect(ratio).toBeGreaterThanOrEqual(AA_NORMAL)
    })

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
          const ratio = contrastRatio(tokens[block]!, tokens[surf]!)
          expect(ratio).toBeGreaterThanOrEqual(AA_BLOCK)
        })
      })
    })
  })
})

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

function composite(fill: string, surface: string, alpha: number): string {
  const f = hexToRgb(fill)
  const s = hexToRgb(surface)
  if (!f || !s) throw new Error(`cannot composite ${fill} over ${surface}`)
  return (
    '#' +
    f
      .map((c, i) =>
        Math.round(c * alpha + s[i]! * (1 - alpha))
          .toString(16)
          .padStart(2, '0'),
      )
      .join('')
  )
}

const BADGE_BACKGROUNDS: [string, (tokens: Record<string, string>) => string][] = [
  ['surface-1', tokens => tokens['--surface-1']!],
  ['surface-2', tokens => tokens['--surface-2']!],
]

describe('badge text on its fill (WCAG AA)', () => {
  describe.each(themeEntries)('theme: %s', (_themeId, tokens) => {
    describe.each(Object.entries(BADGE_TONES))('%s badge', (_tone, spec) => {
      it.each(BADGE_BACKGROUNDS)(`≥ ${AA_NORMAL}:1 on %s and on its fill over it`, (_name, surfaceOf) => {
        const fg = tokens[spec.fg]!
        expect(hexToRgb(fg ?? ''), `${spec.fg} must be a hex colour`).not.toBeNull()
        const surface = surfaceOf(tokens)
        expect(contrastRatio(fg, surface)).toBeGreaterThanOrEqual(AA_NORMAL)
        const fill = composite(tokens[spec.fill]!, surface, spec.fillAlpha)
        expect(contrastRatio(fg, fill)).toBeGreaterThanOrEqual(AA_NORMAL)
      })
    })

    it(`error-foreground on error ≥ ${AA_NORMAL}:1`, () => {
      expect(hexToRgb(tokens['--error-foreground'] ?? ''), '--error-foreground must be a hex colour').not.toBeNull()
      expect(contrastRatio(tokens['--error-foreground']!, tokens['--error']!)).toBeGreaterThanOrEqual(AA_NORMAL)
    })
  })

  it('every theme block sets every text-on-fill token itself (no dark-palette fallback)', () => {
    const own = ['--error-foreground', ...Object.values(BADGE_TONES).map(spec => spec.fg)]
    for (const tok of own) expect(root[tok], `:root missing ${tok}`).toBeTruthy()
    for (const [id, tokens] of Object.entries(perTheme)) {
      for (const tok of own) {
        expect(tokens[tok], `theme "${id}" missing ${tok}`).toBeTruthy()
      }
    }
  })
})
