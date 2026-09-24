import {describe, it, expect} from 'vitest'
import {existsSync, readFileSync} from 'node:fs'
import {createRequire} from 'node:module'
import path from 'node:path'

// The design tokens are CSS custom properties, and two things about them
// broke silently for a long time:
//   - Tailwind cannot add alpha to a bare `var(--x)`, so `bg-brand-500/10`-style
//     tints (282 usages) generated no CSS at all, and a bare `ring-1` fell
//     back to Tailwind's blue;
//   - `--space-1.5` is not a valid custom-property name (no dots in an ident),
//     so browsers dropped the declaration and every half-step spacing utility
//     (p-1.5, gap-1.5, py-0.5, h-3.5) computed to 0.
// This builds the REAL config against a tiny raw document and pins both.

// vitest's jsdom environment gives import.meta.url a non-file scheme, so the
// config is found from the working directory (the package, or the repo root).
function findUp(name: string): string {
  let dir = process.cwd()
  for (;;) {
    const candidate = path.join(dir, name)
    if (existsSync(candidate)) return candidate
    const parent = path.dirname(dir)
    if (parent === dir) throw new Error(`${name} not found above ${process.cwd()}`)
    dir = parent
  }
}

const configPath = findUp(path.join('frontend', 'tailwind.config.js').replace(/^frontend[\\/]/, ''))
const require = createRequire(configPath)
const postcss = require('postcss') as (plugins: unknown[]) => {
  process: (css: string, opts: {from?: string}) => Promise<{css: string}>
}
const tailwindcss = require('tailwindcss') as (config: unknown) => unknown
const loadConfig = require('tailwindcss/loadConfig') as (file: string) => Record<string, unknown>

const indexCss = readFileSync(path.join(path.dirname(configPath), 'src', 'index.css'), 'utf8')

async function build(classes: string): Promise<string> {
  const result = await postcss([
    tailwindcss({
      ...loadConfig(configPath),
      content: [{raw: `<div class="${classes}"></div>`, extension: 'html'}],
      corePlugins: {preflight: false},
    }),
  ]).process('@tailwind base; @tailwind utilities;', {from: undefined})
  return result.css
}

describe('tailwind design tokens', () => {
  it('generates alpha tints for the CSS-variable colours', async () => {
    const css = await build('bg-brand-500/10 ring-brand-500/40 text-text-tertiary/60 border-brand-500/40 bg-brand-500')
    expect(css).toMatch(/\.bg-brand-500\\\/10\s*\{[^}]*var\(--brand-500\)[^}]*0\.1/)
    expect(css).toMatch(/\.ring-brand-500\\\/40\s*\{[^}]*var\(--brand-500\)/)
    expect(css).toMatch(/\.text-text-tertiary\\\/60\s*\{[^}]*var\(--text-tertiary\)/)
    expect(css).toMatch(/\.border-brand-500\\\/40\s*\{[^}]*var\(--brand-500\)/)
    // The plain utility still resolves to the token itself.
    expect(css).toMatch(/\.bg-brand-500\s*\{[^}]*var\(--brand-500\)/)
  })

  it("tints a bare ring with the brand colour, not Tailwind's blue", async () => {
    const css = await build('ring-1')
    expect(css).toMatch(/--tw-ring-color:\s*color-mix\(in srgb, var\(--brand-500\)/)
    expect(css).not.toContain('147 197 253')
    expect(css).not.toContain('59 130 246')
  })

  it('has a backdrop colour for modals and an alias for border-border-default', async () => {
    const css = await build('bg-surface-overlay border-border-default')
    expect(css).toMatch(/\.bg-surface-overlay\s*\{[^}]*var\(--surface-overlay\)/)
    expect(css).toMatch(/\.border-border-default\s*\{[^}]*var\(--border-default\)/)
  })

  it('points half-step spacing at custom properties browsers accept', async () => {
    const css = await build('p-0.5 p-1.5 gap-2.5 h-3.5')
    expect(css).toMatch(/\.p-0\\\.5\s*\{[^}]*var\(--space-0_5\)/)
    expect(css).toMatch(/\.p-1\\\.5\s*\{[^}]*var\(--space-1_5\)/)
    expect(css).toMatch(/\.gap-2\\\.5\s*\{[^}]*var\(--space-2_5\)/)
    expect(css).toMatch(/\.h-3\\\.5\s*\{[^}]*var\(--space-3_5\)/)
    expect(css).not.toMatch(/--space-\d\.\d/)
  })

  it('declares the half-step spacing tokens under valid names in every density', () => {
    expect(indexCss).not.toMatch(/--space-\d\.\d/)
    for (const name of ['0_5', '1_5', '2_5', '3_5']) {
      expect(indexCss).toMatch(new RegExp(`--space-${name}:\\s*\\d+px`))
    }
    // Compact density overrides the same names, not the broken ones.
    const compact = indexCss.slice(indexCss.indexOf("[data-density='compact']"))
    expect(compact).toMatch(/--space-1_5:\s*\d+px/)
    expect(compact).toMatch(/--space-2_5:\s*\d+px/)
    expect(compact).toMatch(/--space-3_5:\s*\d+px/)
  })
})
