import {describe, it, expect} from 'vitest'
import {resolveTypeLabel, stripBlockKeywords, getBlockColor, getBlockBg} from './blocks'

describe('resolveTypeLabel', () => {
  it('returns GOTO for rawType GOTO', () => {
    expect(resolveTypeLabel('ACTION', 'anything', 'GOTO')).toBe('Goto')
  })

  it('returns Label for rawType LABEL', () => {
    expect(resolveTypeLabel('ACTION', 'anything', 'LABEL')).toBe('Label')
  })

  it('returns ForEach Loop for LOOP FOREACH', () => {
    expect(resolveTypeLabel('LOOP', 'LOOP FOREACH item IN list')).toBe('ForEach Loop')
  })

  it('returns While Loop for LOOP WHILE', () => {
    expect(resolveTypeLabel('LOOP', 'LOOP WHILE condition')).toBe('While Loop')
  })

  it('returns human label for known types', () => {
    expect(resolveTypeLabel('ACTION', 'DoSomething')).toBe('Action')
    expect(resolveTypeLabel('CONDITION', 'IF x = 1')).toBe('If')
    expect(resolveTypeLabel('VARIABLE', 'SET x')).toBe('Set Variable')
  })

  it('falls back to type string for unknown types', () => {
    expect(resolveTypeLabel('CUSTOM_TYPE', 'something')).toBe('CUSTOM_TYPE')
  })
})

describe('stripBlockKeywords', () => {
  it('strips leading SWITCH keyword', () => {
    expect(stripBlockKeywords('SWITCH', 'SWITCH myVar')).toBe('myVar')
  })

  it('strips trailing THEN from conditions', () => {
    expect(stripBlockKeywords('CONDITION', 'IF x = 1 THEN')).toBe('x = 1')
  })

  it('strips FOREACH from loops', () => {
    expect(stripBlockKeywords('LOOP', 'FOREACH item IN list')).toBe('item IN list')
  })

  it('strips surrounding % from variable references', () => {
    expect(stripBlockKeywords('ACTION', '%myVar%')).toBe('myVar')
  })

  it('returns name unchanged when no keywords present', () => {
    expect(stripBlockKeywords('ACTION', 'Click button')).toBe('Click button')
  })
})

describe('getBlockColor / getBlockBg', () => {
  it('returns CSS variable for known types', () => {
    expect(getBlockColor('ACTION')).toBe('var(--block-action)')
    expect(getBlockBg('LOOP')).toBe('var(--block-loop-bg)')
  })

  it('returns fallback for unknown types', () => {
    expect(getBlockColor('NONEXISTENT')).toBe('var(--text-tertiary)')
    expect(getBlockBg('NONEXISTENT')).toBe('var(--surface-2)')
  })
})
