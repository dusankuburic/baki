import {describe, it, expect} from 'vitest'
import {render, screen} from '@testing-library/react'
import {Users} from 'lucide-react'
import Badge, {BADGE_TONES, type BadgeTone} from './Badge'

// The Tailwind colour name for each CSS variable a badge fill is mixed from
// (tailwind.config.js), to check that the fill lib/themeContrast.test.ts
// measures is the fill the badge actually paints.
const TAILWIND_COLOUR: Record<string, string> = {
  '--surface-3': 'surface-3',
  '--brand-500': 'brand-500',
  '--info': 'semantic-info',
  '--success': 'semantic-success',
  '--warning': 'semantic-warning',
  '--error': 'semantic-error',
}

const badgeOf = (text: string) => screen.getByText(text).closest('span.relative') as HTMLElement

describe('Badge', () => {
  it('paints each tone with its own per-theme text token', () => {
    const tones = Object.keys(BADGE_TONES) as BadgeTone[]
    render(
      <>
        {tones.map(tone => (
          <Badge key={tone} tone={tone}>
            {tone}
          </Badge>
        ))}
      </>,
    )
    for (const tone of tones) {
      expect(badgeOf(tone).className).toContain(`text-[color:var(--badge-${tone}-fg)]`)
    }
  })

  it('defaults to the neutral tone and the uppercase badge style', () => {
    render(<Badge>API key</Badge>)
    const badge = badgeOf('API key')
    expect(badge.className).toContain('var(--badge-neutral-fg)')
    expect(badge).toHaveClass('uppercase')
  })

  it('can drop the caps for a sentence-case chip', () => {
    render(<Badge caps={false}>Not tested</Badge>)
    expect(badgeOf('Not tested')).not.toHaveClass('uppercase')
  })

  // "SHARED" alone does not say by whom; the screen reader hears the full
  // label once, not the short text and then the label.
  it('gives screen readers the srLabel instead of the short visible text', () => {
    render(
      <Badge tone="brand" icon={Users} srLabel="Shared by Contoso">
        Shared
      </Badge>,
    )
    expect(screen.getByText('Shared')).toHaveAttribute('aria-hidden', 'true')
    expect(screen.getByText('Shared by Contoso')).toHaveClass('sr-only')
    // The sr-only text is contained by the badge, not the nearest scroller.
    expect(badgeOf('Shared')).toHaveClass('relative')
    expect(badgeOf('Shared').querySelector('svg')).toHaveAttribute('aria-hidden', 'true')
  })

  it('shows its title as a hover hint', () => {
    render(<Badge title="Used when you have no default of your own">Default for members</Badge>)
    expect(badgeOf('Default for members')).toHaveAttribute('title', 'Used when you have no default of your own')
  })

  it('paints the fill the contrast test measures', () => {
    for (const [tone, spec] of Object.entries(BADGE_TONES)) {
      const colour = TAILWIND_COLOUR[spec.fill]
      expect(colour, `${tone}: no Tailwind colour for ${spec.fill}`).toBeTruthy()
      const fill = spec.fillAlpha === 1 ? `bg-${colour}` : `bg-${colour}/${Math.round(spec.fillAlpha * 100)}`
      expect(spec.className.split(/\s+/), tone).toContain(fill)
      expect(spec.className, tone).toContain(`text-[color:var(${spec.fg})]`)
    }
  })
})
