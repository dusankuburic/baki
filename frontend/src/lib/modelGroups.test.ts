import {describe, it, expect} from 'vitest'
import {
  compareModelsInGroup,
  compareRegistryModels,
  compareText,
  groupModels,
  isPickerModel,
  isRegistryEntry,
  type GroupableModel,
} from './modelGroups'

const entry = (id: string, over: Partial<GroupableModel> = {}): GroupableModel => ({
  id: `m/${id.padEnd(10, '0')}`,
  displayName: id,
  kind: 'chat',
  purpose: 'general',
  ...over,
})

describe('what the picker lists', () => {
  it('never offers embedding entries', () => {
    expect(isPickerModel({kind: 'embedding'})).toBe(false)
    expect(isPickerModel({kind: 'agent'})).toBe(true)
    expect(isPickerModel({})).toBe(true)
  })

  it('never offers a kind a newer server added: it may not be one the chat can use', () => {
    expect(isPickerModel({kind: 'realtime'})).toBe(false)
    const groups = groupModels([entry('chat1'), entry('voice', {kind: 'realtime'})])
    expect(groups.flatMap(g => g.models.map(m => m.displayName))).toEqual(['chat1'])
  })

  it('tells registry entries from catalogue models', () => {
    expect(isRegistryEntry({id: 'claude-sonnet-4-6'})).toBe(false)
    expect(isRegistryEntry({id: 'm/a1b2c3d4e5'})).toBe(true)
    expect(isRegistryEntry({id: 'gpt-4.1', kind: 'chat'})).toBe(true)
    expect(isRegistryEntry({id: 'gpt-4.1', purpose: 'review'})).toBe(true)
  })
})

describe('ordering', () => {
  it('compares text case- and accent-insensitively, with a stable tie-break', () => {
    expect(compareText('alpha', 'Beta')).toBeLessThan(0)
    expect(compareText('Éclair', 'zeta')).toBeLessThan(0)
    expect(compareText('Docs', 'docs')).toBeLessThan(0)
    expect(compareText('docs', 'Docs')).toBeGreaterThan(0)
    expect(compareText('same', 'same')).toBe(0)
  })

  it("puts the caller's own entries first, then orders by display name, then id", () => {
    const own = entry('zed')
    const shared = entry('alpha', {shared: true})
    expect(compareModelsInGroup(own, shared)).toBeLessThan(0)
    expect(compareModelsInGroup(shared, own)).toBeGreaterThan(0)
    expect(compareModelsInGroup(entry('alpha'), entry('beta'))).toBeLessThan(0)
    const a = {id: 'm/aaaaaaaaaa', displayName: 'Same'}
    const b = {id: 'm/bbbbbbbbbb', displayName: 'Same'}
    expect(compareModelsInGroup(a, b)).toBeLessThan(0)
    expect(compareModelsInGroup(b, a)).toBeGreaterThan(0)
    expect(compareModelsInGroup(a, a)).toBe(0)
    expect(compareModelsInGroup({id: 'm/bbbbbbbbbb'}, {id: 'm/aaaaaaaaaa', displayName: 'zzz'})).toBeLessThan(0)
    expect(compareModelsInGroup({id: 'm/bbbbbbbbbb', displayName: 'aaa'}, {id: 'm/aaaaaaaaaa'})).toBeLessThan(0)
  })

  it('orders the registry by purpose (presets, then custom A–Z), then within the group', () => {
    const docs = entry('docs', {purpose: 'documentation'})
    const general = entry('general')
    const security = entry('sec', {purpose: 'custom', purposeCustom: 'Security'})
    const audit = entry('audit', {purpose: 'custom', purposeCustom: 'audit'})
    const auditShared = entry('audit2', {purpose: 'custom', purposeCustom: 'Audit', shared: true})
    const sorted = [security, docs, auditShared, general, audit].sort((a, b) => compareRegistryModels(a, b))
    expect(sorted).toEqual([general, docs, audit, auditShared, security])
    expect(compareRegistryModels(general, entry('general2'))).toBeLessThan(0)
    expect(compareRegistryModels(security, audit)).toBeGreaterThan(0)
    const pruebas = entry('pruebas', {purpose: 'custom', purposeCustom: 'Pruebas'})
    expect(compareRegistryModels(pruebas, docs, p => (p === 'testing' ? 'Pruebas' : ''))).toBeGreaterThan(0)
    expect(compareRegistryModels(pruebas, security, p => (p === 'testing' ? 'Pruebas' : ''))).toBeLessThan(0)
  })
})

describe('groupModels', () => {
  it('orders the preset groups, then custom groups A–Z, then the plain group', () => {
    const models = [
      entry('tests', {purpose: 'testing'}),
      entry('sec', {purpose: 'custom', purposeCustom: 'Security'}),
      entry('gpt', {purpose: 'general'}),
      entry('arch', {purpose: 'custom', purposeCustom: 'architecture'}),
      entry('docs', {purpose: 'documentation'}),
      entry('rev', {purpose: 'review'}),
      entry('fix', {purpose: 'fixes'}),
      entry('ref', {purpose: 'refactoring'}),
    ]
    const groups = groupModels(models)
    expect(groups.map(g => g.key)).toEqual([
      'general',
      'documentation',
      'fixes',
      'review',
      'refactoring',
      'testing',
      'custom:architecture',
      'custom:security',
    ])
  })

  it('merges custom purposes case-insensitively across personal and shared, named by the first spelling', () => {
    const shared = entry('b-shared', {purpose: 'custom', purposeCustom: 'SECURITY', shared: true})
    const own = entry('z-own', {purpose: 'custom', purposeCustom: 'Security '})
    const [group] = groupModels([shared, own])
    expect(group).toEqual({kind: 'custom', key: 'custom:security', label: 'Security', models: [own, shared]})
    expect(groupModels([own, shared])[0]).toEqual(group)
  })

  it('sorts within a group: own before shared, then display name, then id', () => {
    const b = entry('Beta')
    const a = entry('alpha')
    const s = entry('aardvark', {shared: true})
    const [general] = groupModels([s, b, a])
    expect(general!.models).toEqual([a, b, s])
  })

  it('never lists embeddings and keeps catalogue models in listing order', () => {
    const embed = entry('embed', {kind: 'embedding'})
    const opus = {id: 'claude-opus-4-1', displayName: 'Claude Opus 4.1'}
    const haiku = {id: 'claude-haiku-4-5', displayName: 'Claude Haiku 4.5'}
    const groups = groupModels([opus, embed, haiku])
    expect(groups).toEqual([{kind: 'plain', key: 'models', models: [opus, haiku]}])
  })

  it('files an entry without a purpose under General and custom text naming a preset under it', () => {
    const bare = {id: 'm/bare000000', displayName: 'Bare', kind: 'chat'}
    const named = entry('named', {purpose: 'custom', purposeCustom: 'Review & analysis'})
    const local = entry('local', {purpose: 'custom', purposeCustom: 'Revisión'})
    const groups = groupModels([bare, named, local], {labelOf: p => (p === 'review' ? 'Revisión' : '')})
    expect(groups.map(g => [g.key, g.models.length])).toEqual([
      ['general', 1],
      ['review', 2],
    ])
  })

  it('returns no groups for an empty listing', () => {
    expect(groupModels([])).toEqual([])
  })
})
