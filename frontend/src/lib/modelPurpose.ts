import {
  AZURE_MODEL_KINDS,
  isKnown,
  type AzureModelKind,
  type ModelPurpose,
  type ModelPurposePreset,
  type Tolerant,
} from '@/types'
import {common} from '@/i18n/en'

const PRESET_RANK = {
  general: 0,
  documentation: 1,
  fixes: 2,
  review: 3,
  refactoring: 4,
  testing: 5,
} as const satisfies Record<ModelPurposePreset, number>

export const PURPOSE_PRESETS: readonly ModelPurposePreset[] = (Object.keys(PRESET_RANK) as ModelPurposePreset[]).sort(
  (a, b) => PRESET_RANK[a] - PRESET_RANK[b],
)

export const DEFAULT_PURPOSE: ModelPurposePreset = 'general'

export const PURPOSE_CUSTOM_MAX_LENGTH = 32

export function isPurposePreset(value: unknown): value is ModelPurposePreset {
  return typeof value === 'string' && Object.prototype.hasOwnProperty.call(PRESET_RANK, value)
}

export function purposeRank(preset: ModelPurposePreset): number {
  return PRESET_RANK[preset]
}

export type PurposeLabelKey = `models.purpose.${ModelPurpose}`
export function purposeLabelKey(purpose: ModelPurpose): PurposeLabelKey {
  return `models.purpose.${purpose}`
}

export type ModelKindLabelKey = `models.kind.${AzureModelKind | 'other'}`
export function modelKindLabelKey(kind: Tolerant<AzureModelKind>): ModelKindLabelKey {
  return isKnown(AZURE_MODEL_KINDS, kind) ? `models.kind.${kind}` : 'models.kind.other'
}

export type PurposeLabelOf = (preset: ModelPurposePreset) => string

export function charLength(text: string): number {
  return [...text].length
}

export function cleanPurposeText(text: string): string {
  return text.normalize('NFC').replace(/\s+/g, ' ').trim()
}

function fold(text: string): string {
  return cleanPurposeText(text).toLowerCase()
}

export function matchPurposePreset(text: string, labelOf?: PurposeLabelOf): ModelPurposePreset | null {
  const want = fold(text)
  if (!want) return null
  for (const preset of PURPOSE_PRESETS) {
    if (want === preset) return preset
    if (want === fold(common.models.purpose[preset])) return preset
    if (labelOf && want === fold(labelOf(preset))) return preset
  }
  return null
}

export type PurposeValue = {purpose: ModelPurposePreset} | {purpose: 'custom'; purposeCustom: string}

export type PurposeInputResult = ({ok: true} & PurposeValue) | {ok: false; error: 'required' | 'tooLong'}

export function normalizePurposeInput(
  purpose: ModelPurpose,
  customText: string,
  labelOf?: PurposeLabelOf,
): PurposeInputResult {
  if (purpose !== 'custom') return {ok: true, purpose}
  const text = cleanPurposeText(customText)
  if (!text) return {ok: false, error: 'required'}
  const preset = matchPurposePreset(text, labelOf)
  if (preset) return {ok: true, purpose: preset}
  if (charLength(text) > PURPOSE_CUSTOM_MAX_LENGTH) return {ok: false, error: 'tooLong'}
  return {ok: true, purpose: 'custom', purposeCustom: text}
}

export type ResolvedPurpose = {kind: 'preset'; preset: ModelPurposePreset} | {kind: 'custom'; text: string}

export function resolvePurpose(
  entry: {purpose?: string | null; purposeCustom?: string | null},
  labelOf?: PurposeLabelOf,
): ResolvedPurpose {
  const purpose = entry.purpose ?? ''
  if (isPurposePreset(purpose)) return {kind: 'preset', preset: purpose}
  const text = cleanPurposeText((purpose === 'custom' ? entry.purposeCustom : entry.purposeCustom || purpose) ?? '')
  if (!text) return {kind: 'preset', preset: DEFAULT_PURPOSE}
  const preset = matchPurposePreset(text, labelOf)
  return preset ? {kind: 'preset', preset} : {kind: 'custom', text}
}

export function purposeLabel(
  entry: {purpose?: string | null; purposeCustom?: string | null},
  labelOf: PurposeLabelOf,
): string {
  const p = resolvePurpose(entry, labelOf)
  return p.kind === 'preset' ? labelOf(p.preset) : p.text
}

export function purposeGroupKey(
  entry: {purpose?: string | null; purposeCustom?: string | null},
  labelOf?: PurposeLabelOf,
): string {
  const p = resolvePurpose(entry, labelOf)
  return p.kind === 'preset' ? p.preset : `custom:${p.text.toLowerCase()}`
}
