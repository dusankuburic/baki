import {describe, it, expect} from 'vitest'
import i18next from '@/i18n'
import {unknownValue} from '@/testing/testHelpers'
import {
  DEFAULT_PURPOSE,
  PURPOSE_CUSTOM_MAX_LENGTH,
  PURPOSE_PRESETS,
  charLength,
  cleanPurposeText,
  isPurposePreset,
  matchPurposePreset,
  modelKindLabelKey,
  normalizePurposeInput,
  purposeGroupKey,
  purposeLabel,
  purposeLabelKey,
  purposeRank,
  resolvePurpose,
} from './modelPurpose'

describe('purpose presets', () => {
  it('lists the six presets in picker order, General first', () => {
    expect(PURPOSE_PRESETS).toEqual(['general', 'documentation', 'fixes', 'review', 'refactoring', 'testing'])
    expect(DEFAULT_PURPOSE).toBe('general')
    expect(PURPOSE_PRESETS.map(purposeRank)).toEqual([0, 1, 2, 3, 4, 5])
  })

  it('tells presets from everything else', () => {
    expect(isPurposePreset('review')).toBe(true)
    expect(isPurposePreset('custom')).toBe(false)
    expect(isPurposePreset('toString')).toBe(false)
    expect(isPurposePreset(3)).toBe(false)
  })

  // The labels are defined once, in the common namespace.
  it('has a label for every purpose and kind', () => {
    for (const p of [...PURPOSE_PRESETS, 'custom' as const]) {
      const key = purposeLabelKey(p)
      expect(i18next.t(key, {ns: 'common'})).not.toBe(key)
    }
    expect(i18next.t(purposeLabelKey('fixes'), {ns: 'common'})).toBe('Fixes & debugging')
    for (const k of ['chat', 'claude', 'agent', 'embedding'] as const) {
      const key = modelKindLabelKey(k)
      expect(i18next.t(key, {ns: 'common'})).not.toBe(key)
    }
  })

  it('labels a kind a newer server added neutrally, not with its raw key', () => {
    expect(modelKindLabelKey(unknownValue('realtime'))).toBe('models.kind.other')
    expect(i18next.t(modelKindLabelKey(unknownValue('realtime')), {ns: 'common'})).toBe('Other')
  })
})

describe('custom purpose text', () => {
  it('is NFC-normalised, trimmed and has inner whitespace collapsed', () => {
    // "é" as e + combining acute becomes the single precomposed character.
    expect(cleanPurposeText('  Cafe\u0301 \t  docs\n ')).toBe('Caf\u00e9 docs')
    expect(cleanPurposeText('   ')).toBe('')
  })

  it('counts characters, not UTF-16 units', () => {
    expect(charLength('😀😀')).toBe(2)
    expect('😀😀'.length).toBe(4)
  })

  it('matches a preset by key, English label or the UI label, case-insensitively', () => {
    expect(matchPurposePreset('REVIEW')).toBe('review')
    expect(matchPurposePreset('  fixes   &  DEBUGGING ')).toBe('fixes')
    expect(matchPurposePreset('Dokumentation')).toBeNull()
    expect(matchPurposePreset('Dokumentation', p => (p === 'documentation' ? 'Dokumentation' : p))).toBe(
      'documentation',
    )
    expect(matchPurposePreset('Security')).toBeNull()
    expect(matchPurposePreset('')).toBeNull()
  })
})

describe('normalizePurposeInput', () => {
  it('passes a preset through', () => {
    expect(normalizePurposeInput('testing', 'ignored')).toEqual({ok: true, purpose: 'testing'})
  })

  it('stores custom text cleaned', () => {
    expect(normalizePurposeInput('custom', '  Security   audits ')).toEqual({
      ok: true,
      purpose: 'custom',
      purposeCustom: 'Security audits',
    })
  })

  it('stores custom text that names a preset as that preset', () => {
    expect(normalizePurposeInput('custom', 'documentation')).toEqual({ok: true, purpose: 'documentation'})
    expect(normalizePurposeInput('custom', 'Review & Analysis')).toEqual({ok: true, purpose: 'review'})
  })

  it('requires custom text and caps it at 32 characters', () => {
    expect(normalizePurposeInput('custom', ' \t ')).toEqual({ok: false, error: 'required'})
    const max = 'x'.repeat(PURPOSE_CUSTOM_MAX_LENGTH)
    expect(normalizePurposeInput('custom', max)).toEqual({ok: true, purpose: 'custom', purposeCustom: max})
    expect(normalizePurposeInput('custom', `${max}y`)).toEqual({ok: false, error: 'tooLong'})
    // 32 emoji are 32 characters (64 UTF-16 units).
    expect(normalizePurposeInput('custom', '😀'.repeat(32)).ok).toBe(true)
  })
})

describe('resolvePurpose and purposeGroupKey', () => {
  it('reads presets, custom text and the General default', () => {
    expect(resolvePurpose({purpose: 'refactoring'})).toEqual({kind: 'preset', preset: 'refactoring'})
    expect(resolvePurpose({purpose: 'custom', purposeCustom: ' Security '})).toEqual({kind: 'custom', text: 'Security'})
    expect(resolvePurpose({})).toEqual({kind: 'preset', preset: 'general'})
    expect(resolvePurpose({purpose: null, purposeCustom: null})).toEqual({kind: 'preset', preset: 'general'})
    expect(resolvePurpose({purpose: 'custom', purposeCustom: '  '})).toEqual({kind: 'preset', preset: 'general'})
    expect(resolvePurpose({purpose: 'custom'})).toEqual({kind: 'preset', preset: 'general'})
  })

  it('folds custom text naming a preset into it', () => {
    expect(resolvePurpose({purpose: 'custom', purposeCustom: 'Testing'})).toEqual({kind: 'preset', preset: 'testing'})
    // …and so does a label in the UI language.
    expect(resolvePurpose({purpose: 'custom', purposeCustom: 'Tests'}, p => (p === 'testing' ? 'Tests' : ''))).toEqual({
      kind: 'preset',
      preset: 'testing',
    })
    expect(resolvePurpose({purpose: 'custom', purposeCustom: 'Tests'})).toEqual({kind: 'custom', text: 'Tests'})
  })

  it('shows a purpose a newer server added as custom text instead of dropping it', () => {
    expect(resolvePurpose({purpose: 'security'})).toEqual({kind: 'custom', text: 'security'})
    expect(resolvePurpose({purpose: 'security', purposeCustom: 'Security & compliance'})).toEqual({
      kind: 'custom',
      text: 'Security & compliance',
    })
  })

  it('groups presets by key and custom text case-insensitively', () => {
    expect(purposeGroupKey({purpose: 'review'})).toBe('review')
    expect(purposeGroupKey({purpose: 'custom', purposeCustom: 'Security'})).toBe('custom:security')
    expect(purposeGroupKey({purpose: 'custom', purposeCustom: ' SECURITY '})).toBe('custom:security')
    expect(purposeGroupKey({purpose: 'custom', purposeCustom: 'general'})).toBe('general')
  })
})

describe('purposeLabel', () => {
  const labelOf = (p: Parameters<typeof purposeLabelKey>[0]) => i18next.t(purposeLabelKey(p), {ns: 'common'})

  it('labels a preset in the UI language and a custom purpose by its text', () => {
    expect(purposeLabel({purpose: 'documentation'}, labelOf)).toBe('Documentation')
    expect(purposeLabel({purpose: 'custom', purposeCustom: 'Security'}, labelOf)).toBe('Security')
    // Missing means General; custom text naming a preset is that preset.
    expect(purposeLabel({}, labelOf)).toBe('General')
    expect(purposeLabel({purpose: 'custom', purposeCustom: 'fixes & DEBUGGING'}, labelOf)).toBe('Fixes & debugging')
  })
})
