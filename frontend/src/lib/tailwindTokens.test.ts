import {describe, it, expect} from 'vitest'
import {readFileSync} from 'node:fs'
import {createRequire} from 'node:module'
import path from 'node:path'

const frontendRoot = process.cwd().includes(path.join('frontend'))
  ? process.cwd()
  : path.join(process.cwd(), 'frontend')
const indexCss = readFileSync(path.join(frontendRoot, 'src', 'index.css'), 'utf8')

const require = createRequire(path.join(frontendRoot, 'package.json'))
const postcss = require('postcss') as (plugins: unknown[]) => {
  process: (css: string, opts: {from?: string}) => Promise<{css: string}>
}
const tailwindPostcss = require('@tailwindcss/postcss') as () => unknown

async function build(classes: string): Promise<string> {
  const probe = `@import './src/index.css';\n@source inline("${classes}");\n`
  const result = await postcss([tailwindPostcss()]).process(probe, {
    from: path.join(frontendRoot, 'probe.css'),
  })
  return result.css
}

describe('tailwind design tokens (v4)', () => {
  it('generates alpha tints for the CSS-variable colours', async () => {
    const css = await build('bg-brand-500/10 ring-brand-500/40 text-text-tertiary/60 border-brand-500/40 bg-brand-500')
    expect(css).toMatch(/\.bg-brand-500\\\/10\s*\{[^}]*color-mix\([^)]*var\(--color-brand-500\)/)
    expect(css).toMatch(/\.ring-brand-500\\\/40\s*\{[^}]*var\(--color-brand-500\)/)
    expect(css).toMatch(/\.text-text-tertiary\\\/60\s*\{[^}]*var\(--color-text-tertiary\)/)
    expect(css).toMatch(/\.border-brand-500\\\/40\s*\{[^}]*var\(--color-brand-500\)/)
    expect(css).toMatch(/\.bg-brand-500\s*\{[^}]*var\(--color-brand-500\)/)
  })

  it("tints a bare ring-1 with the brand colour, not the browser's currentColor", async () => {
    const css = await build('ring-1')
    expect(css).toMatch(/--tw-ring-color:[^;]*var\(--brand-500\)/)
    expect(css).not.toContain('147 197 253')
    expect(css).not.toContain('59 130 246')
  })

  it('has a backdrop colour for modals and an alias for border-border-default', async () => {
    const css = await build('bg-surface-overlay border-border-default')
    expect(css).toMatch(/\.bg-surface-overlay\s*\{[^}]*var\(--color-surface-overlay\)/)
    expect(css).toMatch(/\.border-border-default\s*\{[^}]*var\(--color-border-default\)/)
  })

  it('points half-step spacing at custom properties browsers accept', async () => {
    const css = await build('p-0.5 p-1.5 gap-2.5 h-3.5')
    expect(css).toMatch(/\.p-0\\\.5\s*\{[^}]*var\(--spacing-0_5\)/)
    expect(css).toMatch(/\.p-1\\\.5\s*\{[^}]*var\(--spacing-1_5\)/)
    expect(css).toMatch(/\.gap-2\\\.5\s*\{[^}]*var\(--spacing-2_5\)/)
    expect(css).toMatch(/\.h-3\\\.5\s*\{[^}]*var\(--spacing-3_5\)/)
  })

  it('keeps every dark theme on the dark variant selector', async () => {
    const css = await build('dark:bg-brand-500')
    expect(css).toMatch(/\[data-theme="dark"\]/)
    expect(css).toMatch(/\[data-theme="everforest"\]/)
    expect(css).not.toMatch(/prefers-color-scheme/)
  })

  it('declares the half-step spacing tokens under valid names in every density', () => {
    expect(indexCss).not.toMatch(/--space-\d\.\d/)
    for (const name of ['0_5', '1_5', '2_5', '3_5']) {
      expect(indexCss).toMatch(new RegExp(`--space-${name}:\\s*\\d+px`))
    }
    const compact = indexCss.slice(indexCss.indexOf("[data-density='compact']"))
    expect(compact).toMatch(/--space-1_5:\s*\d+px/)
  })
})
