import type {AzureModelKind, ModelPurpose, ModelPurposePreset} from '@/types'
import {common} from '@/i18n/en'

// What a registry model is for. A purpose is stored as a preset key or as
// 'custom' plus text; the chat picker groups models by it (see modelGroups).
// The labels live once in en.ts (`common.models.purpose.*`); helpers here
// return keys, never copy.

// Picker order. `satisfies` makes a preset added to ModelPurposePreset a
// compile error here until it has a place.
const PRESET_RANK = {
  general: 0,
  documentation: 1,
  fixes: 2,
  review: 3,
  refactoring: 4,
  testing: 5,
} as const satisfies Record<ModelPurposePreset, number>

/** The preset purposes in their fixed picker order. */
export const PURPOSE_PRESETS: readonly ModelPurposePreset[] = (Object.keys(PRESET_RANK) as ModelPurposePreset[]).sort(
  (a, b) => PRESET_RANK[a] - PRESET_RANK[b],
)

/** A new model's purpose. */
export const DEFAULT_PURPOSE: ModelPurposePreset = 'general'

/** A custom purpose's text is at most this many characters (code points, as the server counts them). */
export const PURPOSE_CUSTOM_MAX_LENGTH = 32

export function isPurposePreset(value: unknown): value is ModelPurposePreset {
  return typeof value === 'string' && Object.prototype.hasOwnProperty.call(PRESET_RANK, value)
}

/** A preset's position in the picker (0 = General). */
export function purposeRank(preset: ModelPurposePreset): number {
  return PRESET_RANK[preset]
}

/** `common` namespace key of a purpose's label ("Custom…" for the custom choice itself). */
export type PurposeLabelKey = `models.purpose.${ModelPurpose}`
export function purposeLabelKey(purpose: ModelPurpose): PurposeLabelKey {
  return `models.purpose.${purpose}`
}

/** `common` namespace key of a kind's label. */
export type ModelKindLabelKey = `models.kind.${AzureModelKind}`
export function modelKindLabelKey(kind: AzureModelKind): ModelKindLabelKey {
  return `models.kind.${kind}`
}

/**
 * A preset's label in the UI language — e.g. `p => t(purposeLabelKey(p),
 * {ns: 'common'})`. Custom text matching it (case-insensitively) is stored as
 * that preset.
 */
export type PurposeLabelOf = (preset: ModelPurposePreset) => string

/** Length in characters (code points), not UTF-16 units — the server's count. */
export function charLength(text: string): number {
  return [...text].length
}

/** Custom purpose text as stored: NFC-normalised, trimmed, inner whitespace collapsed to one space. */
export function cleanPurposeText(text: string): string {
  return text.normalize('NFC').replace(/\s+/g, ' ').trim()
}

function fold(text: string): string {
  return cleanPurposeText(text).toLowerCase()
}

/**
 * The preset whose key or label the text names, compared case-insensitively
 * after cleaning: the English label always counts, and so does `labelOf`'s
 * (the UI language's) when given. Null when none does.
 */
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

/** A purpose as sent to the server. */
export type PurposeValue = {purpose: ModelPurposePreset} | {purpose: 'custom'; purposeCustom: string}

export type PurposeInputResult = ({ok: true} & PurposeValue) | {ok: false; error: 'required' | 'tooLong'}

/**
 * Normalises the purpose field of the model form. A preset passes through.
 * Custom text is cleaned (cleanPurposeText); text that names a preset
 * becomes that preset; otherwise it must be 1–32 characters.
 */
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

/** A stored purpose as the UI shows it. */
export type ResolvedPurpose = {kind: 'preset'; preset: ModelPurposePreset} | {kind: 'custom'; text: string}

/**
 * Reads a stored entry's purpose. Missing or blank means General; custom
 * text that names a preset counts as that preset; an unknown value (a preset
 * a newer server added) shows as custom text rather than vanishing.
 */
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

/**
 * A stored purpose as a label in the UI language: a preset's through
 * `labelOf` (e.g. `p => t(purposeLabelKey(p), {ns: 'common'})`), a custom
 * one's own text.
 */
export function purposeLabel(
  entry: {purpose?: string | null; purposeCustom?: string | null},
  labelOf: PurposeLabelOf,
): string {
  const p = resolvePurpose(entry, labelOf)
  return p.kind === 'preset' ? labelOf(p.preset) : p.text
}

/**
 * The picker group an entry falls in: its preset key, or "custom:" plus the
 * case-folded text, so "Security" and "security " share one group whoever
 * wrote them.
 */
export function purposeGroupKey(
  entry: {purpose?: string | null; purposeCustom?: string | null},
  labelOf?: PurposeLabelOf,
): string {
  const p = resolvePurpose(entry, labelOf)
  return p.kind === 'preset' ? p.preset : `custom:${p.text.toLowerCase()}`
}
