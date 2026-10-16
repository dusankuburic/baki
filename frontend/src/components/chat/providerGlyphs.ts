import type {ComponentType} from 'react'
import {Cloud, HardDrive, KeyRound} from 'lucide-react'
import {GithubMark} from '@/components/shared'

type Glyph = ComponentType<{size?: number}>

// The glyph of a provider by the catalogue's `icon` (internal/ai/spec_*.go), the one map the model picker and the
// device-login button draw from. `drive` is what the local provider's spec sends; `hard-drive` and `key` are the
// names the client used before the catalogue.
export const PROVIDER_GLYPHS: Readonly<Record<string, Glyph>> = {
  cloud: Cloud,
  drive: HardDrive,
  'hard-drive': HardDrive,
  github: GithubMark,
  key: KeyRound,
}

/** Whether PROVIDER_GLYPHS draws a catalogue icon name (own keys only: an icon named like a prototype key does not). */
export function hasProviderGlyph(icon: string | undefined): icon is string {
  return !!icon && Object.prototype.hasOwnProperty.call(PROVIDER_GLYPHS, icon)
}
