import {CHAT_MODEL_KINDS, isKnown, type ModelPurposePreset} from '@/types'
import {PURPOSE_PRESETS, purposeRank, resolvePurpose, type PurposeLabelOf} from './modelPurpose'
import {isRegistryModelId} from './modelIds'

export interface GroupableModel {
  id: string
  displayName?: string
  kind?: string
  purpose?: string | null
  purposeCustom?: string | null
  shared?: boolean
}

export type ModelGroup<M extends GroupableModel = GroupableModel> =
  | {kind: 'preset'; key: ModelPurposePreset; models: M[]}
  | {kind: 'custom'; key: string; label: string; models: M[]}
  | {kind: 'plain'; key: 'models'; models: M[]}

export function isPickerModel(model: Pick<GroupableModel, 'kind'>): boolean {
  return !model.kind || isKnown(CHAT_MODEL_KINDS, model.kind)
}

export function isRegistryEntry(model: GroupableModel): boolean {
  return !!model.kind || !!model.purpose || isRegistryModelId(model.id)
}

export function compareText(a: string, b: string): number {
  const byLocale = a.localeCompare(b, undefined, {sensitivity: 'base'})
  if (byLocale !== 0) return byLocale
  return a < b ? -1 : a > b ? 1 : 0
}

export function compareModelsInGroup(a: GroupableModel, b: GroupableModel): number {
  if (!!a.shared !== !!b.shared) return a.shared ? 1 : -1
  const byName = compareText(a.displayName || a.id, b.displayName || b.id)
  if (byName !== 0) return byName
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0
}

export function compareRegistryModels(a: GroupableModel, b: GroupableModel, labelOf?: PurposeLabelOf): number {
  const [rankA, textA] = purposeOrder(a, labelOf)
  const [rankB, textB] = purposeOrder(b, labelOf)
  if (rankA !== rankB) return rankA - rankB
  const byText = compareText(textA, textB)
  if (byText !== 0) return byText
  return compareModelsInGroup(a, b)
}

function purposeOrder(model: GroupableModel, labelOf?: PurposeLabelOf): [number, string] {
  const p = resolvePurpose(model, labelOf)
  return p.kind === 'preset' ? [purposeRank(p.preset), ''] : [PURPOSE_PRESETS.length, p.text.toLowerCase()]
}

export function groupModels<M extends GroupableModel>(
  models: readonly M[],
  opts: {labelOf?: PurposeLabelOf} = {},
): ModelGroup<M>[] {
  const presets = new Map<ModelPurposePreset, M[]>()
  const customs = new Map<string, {model: M; text: string}[]>()
  const plain: M[] = []
  for (const model of models) {
    if (!isPickerModel(model)) continue
    if (!isRegistryEntry(model)) {
      plain.push(model)
      continue
    }
    const purpose = resolvePurpose(model, opts.labelOf)
    if (purpose.kind === 'preset') {
      const members = presets.get(purpose.preset) ?? []
      members.push(model)
      presets.set(purpose.preset, members)
    } else {
      const key = `custom:${purpose.text.toLowerCase()}`
      const members = customs.get(key) ?? []
      members.push({model, text: purpose.text})
      customs.set(key, members)
    }
  }

  const groups: ModelGroup<M>[] = []
  for (const preset of PURPOSE_PRESETS) {
    const members = presets.get(preset)
    if (members) groups.push({kind: 'preset', key: preset, models: [...members].sort(compareModelsInGroup)})
  }
  const customGroups: Extract<ModelGroup<M>, {kind: 'custom'}>[] = []
  for (const [key, members] of customs) {
    const sorted = [...members].sort((a, b) => compareModelsInGroup(a.model, b.model))
    const first = sorted[0]
    if (first) customGroups.push({kind: 'custom', key, label: first.text, models: sorted.map(m => m.model)})
  }
  customGroups.sort((a, b) => compareText(a.label, b.label))
  groups.push(...customGroups)
  if (plain.length > 0) groups.push({kind: 'plain', key: 'models', models: plain})
  return groups
}
