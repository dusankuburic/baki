import type {ModelPurposePreset} from '@/types'
import {PURPOSE_PRESETS, purposeRank, resolvePurpose, type PurposeLabelOf} from './modelPurpose'
import {isRegistryModelId} from './modelIds'

// The chat model picker's groups. Registry entries group by purpose: the six
// presets in their fixed order, then custom purposes A–Z (one group per
// case-insensitive text across personal and shared entries, named by the
// first spelling), then a plain "Models" group holding everything without a
// purpose — a catalogue provider's models, which keep the order the server
// lists them in. Embedding entries are never offered.

/** The fields grouping reads; ModelDetail fits. */
export interface GroupableModel {
  id: string
  displayName?: string
  kind?: string
  purpose?: string | null
  purposeCustom?: string | null
  shared?: boolean
}

export type ModelGroup<M extends GroupableModel = GroupableModel> =
  /** A preset purpose; the UI labels it with purposeLabelKey(key). */
  | {kind: 'preset'; key: ModelPurposePreset; models: M[]}
  /** A custom purpose; `label` is its first spelling. */
  | {kind: 'custom'; key: string; label: string; models: M[]}
  /** Models without a purpose (catalogue providers). */
  | {kind: 'plain'; key: 'models'; models: M[]}

/** Whether the picker offers the model at all: embedding entries never. */
export function isPickerModel(model: Pick<GroupableModel, 'kind'>): boolean {
  return model.kind !== 'embedding'
}

/** A registry entry — it has a kind or a purpose, or an "m/" id — rather than a catalogue model. */
export function isRegistryEntry(model: GroupableModel): boolean {
  return !!model.kind || !!model.purpose || isRegistryModelId(model.id)
}

/** Case-insensitive, locale-aware text order with a code-unit tie-break, so the result is total. */
export function compareText(a: string, b: string): number {
  const byLocale = a.localeCompare(b, undefined, {sensitivity: 'base'})
  if (byLocale !== 0) return byLocale
  return a < b ? -1 : a > b ? 1 : 0
}

/** Order inside a group: the caller's own entries before shared ones, then display name, then id. */
export function compareModelsInGroup(a: GroupableModel, b: GroupableModel): number {
  if (!!a.shared !== !!b.shared) return a.shared ? 1 : -1
  const byName = compareText(a.displayName || a.id, b.displayName || b.id)
  if (byName !== 0) return byName
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0
}

/**
 * The registry's display order (AI Accounts lists, the store's cache):
 * purpose — presets in picker order, then custom purposes A–Z — then
 * compareModelsInGroup.
 */
export function compareRegistryModels(a: GroupableModel, b: GroupableModel, labelOf?: PurposeLabelOf): number {
  const [rankA, textA] = purposeOrder(a, labelOf)
  const [rankB, textB] = purposeOrder(b, labelOf)
  if (rankA !== rankB) return rankA - rankB
  const byText = compareText(textA, textB)
  if (byText !== 0) return byText
  return compareModelsInGroup(a, b)
}

// A purpose's place: a preset's rank, or — after every preset — the custom
// text, case-folded.
function purposeOrder(model: GroupableModel, labelOf?: PurposeLabelOf): [number, string] {
  const p = resolvePurpose(model, labelOf)
  return p.kind === 'preset' ? [purposeRank(p.preset), ''] : [PURPOSE_PRESETS.length, p.text.toLowerCase()]
}

/**
 * Groups a provider's listing for the picker (see the header comment).
 * `labelOf` lets custom text that names a preset in the UI language join
 * that preset's group.
 */
export function groupModels<M extends GroupableModel>(
  models: readonly M[],
  opts: {labelOf?: PurposeLabelOf} = {},
): ModelGroup<M>[] {
  const presets = new Map<ModelPurposePreset, M[]>()
  // Each member with its own spelling of the custom text.
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
    // Named by the first spelling in the group's own order (personal first),
    // so the name doesn't depend on the order the server listed entries in.
    // Every group has at least one member (pushed before the map set).
    const first = sorted[0]
    if (first) customGroups.push({kind: 'custom', key, label: first.text, models: sorted.map(m => m.model)})
  }
  customGroups.sort((a, b) => compareText(a.label, b.label))
  groups.push(...customGroups)
  if (plain.length > 0) groups.push({kind: 'plain', key: 'models', models: plain})
  return groups
}
