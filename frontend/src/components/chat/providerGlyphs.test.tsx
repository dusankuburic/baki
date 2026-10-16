import {describe, it, expect, vi} from 'vitest'
import {render, act} from '@testing-library/react'
import ModelPicker from './ModelPicker'
import DeviceAuthLoginButton from './DeviceAuthLoginButton'

vi.mock('@/platform/adapters', () => ({createAdapter: () => ({openURL: vi.fn()})}))

// What a glyph draws, whatever its size: the lucide class (none for the GitHub mark) and its first path. A selector
// that missed would make two glyphs compare equal as nothing at all, so both must be there.
function drawing(svg: Element | null): string {
  expect(svg, 'the glyph selector found no svg').not.toBeNull()
  const path = svg!.querySelector('path')
  expect(path, 'the glyph has no path').not.toBeNull()
  return `${svg!.getAttribute('class') ?? ''}|${path!.getAttribute('d')}`
}

function pickerGlyph(icon: string): string {
  const {container, unmount} = render(
    <ModelPicker
      providers={[{id: 'acme', name: 'Acme', icon, models: []}]}
      provider="acme"
      model=""
      label=""
      defaults={{}}
      onSelect={vi.fn()}
    />,
  )
  const out = drawing(container.querySelector('button[aria-haspopup="dialog"] svg:not(.lucide-chevron-down)'))
  unmount()
  return out
}

async function deviceGlyph(icon: string): Promise<string> {
  const {container, unmount} = render(
    <DeviceAuthLoginButton
      provider={{
        name: 'acme',
        label: 'Sign in with Acme',
        buttonClassName: '',
        icon,
        getUser: () => Promise.resolve(null),
        startAuth: () => Promise.resolve(null),
        pollAuth: () => Promise.resolve({status: 'pending'}),
        revokeAuth: () => Promise.resolve(),
      }}
    />,
  )
  await act(async () => {})
  const out = drawing(container.querySelector('button[data-glyph] svg'))
  unmount()
  return out
}

describe('provider glyphs', () => {
  // The icon names the provider specs send (internal/ai/spec_*.go), plus the ones the client has always drawn.
  it.each(['cloud', 'github', 'drive', 'hard-drive', 'key'])(
    'the model picker and the device-login button draw %s alike',
    async icon => {
      expect(pickerGlyph(icon)).toBe(await deviceGlyph(icon))
    },
  )

  it("draws the local provider's spec icon as a drive", async () => {
    expect(pickerGlyph('drive')).toContain('lucide-hard-drive')
    expect(await deviceGlyph('drive')).toContain('lucide-hard-drive')
  })
})
