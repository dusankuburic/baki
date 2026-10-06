import {describe, it, expect} from 'vitest'
import {readFileSync} from 'node:fs'
import {THEME_REGISTRY} from './themeRegistry'

// The chart palette lives in index.css as --viz-* tokens: the dark steps in
// :root and the light steps in one rule that lists every light theme. This
// keeps that rule honest — a light theme added to the registry without a
// --viz entry would silently chart in the dark palette.
const css = readFileSync('src/index.css', 'utf-8')

function tokensIn(block: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const m of block.matchAll(/(--viz-[\w-]+):\s*([^;]+);/g)) out[m[1]!] = m[2]!.trim()
  return out
}

const rootBlock = css.match(/^:root\s*\{([^}]+)\}/m)![1]!
const lightRuleMatch = css.match(/:is\(([^)]*\[data-theme=[^)]*)\)\s*\{([^}]+)\}/)

describe('data-viz tokens', () => {
  it(':root defines the eight series slots and the ordinal ramp as hex', () => {
    const root = tokensIn(rootBlock)
    for (let i = 1; i <= 8; i++) expect(root[`--viz-${i}`]).toMatch(/^#[0-9a-f]{6}$/i)
    for (let i = 1; i <= 4; i++) expect(root[`--viz-seq-${i}`]).toMatch(/^#[0-9a-f]{6}$/i)
    for (const name of [
      '--viz-emphasis',
      '--viz-muted',
      '--viz-grid',
      '--viz-axis',
      '--viz-tick',
      '--viz-tooltip-bg',
    ]) {
      expect(root[name]).toMatch(/^var\(--[\w-]+\)$/)
    }
  })

  it('the light-theme rule covers every light theme in the registry with its own steps', () => {
    expect(lightRuleMatch).not.toBeNull()
    const [, selector, body] = lightRuleMatch!
    const lightIds = THEME_REGISTRY.filter(t => t.category === 'light').map(t => t.id)
    expect(lightIds.length).toBeGreaterThan(0)
    for (const id of lightIds) expect(selector).toContain(`[data-theme='${id}']`)
    const light = tokensIn(body!)
    const root = tokensIn(rootBlock)
    for (let i = 1; i <= 8; i++) {
      expect(light[`--viz-${i}`]).toMatch(/^#[0-9a-f]{6}$/i)
    }
    // The steps differ from the dark ones (same hues, stepped for a light surface).
    expect(light['--viz-1']).not.toBe(root['--viz-1'])
  })
})
