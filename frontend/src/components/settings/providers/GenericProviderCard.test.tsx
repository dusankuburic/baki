import {describe, it, expect} from 'vitest'
import {helpHref} from './GenericProviderCard'

// F5: the help link is the only anchor built from a server string.
describe('helpHref', () => {
  it('offers http(s) targets and nothing else', () => {
    expect(helpHref('https://docs.example.com/mistral')).toBe('https://docs.example.com/mistral')
    expect(helpHref('http://docs.example.com/x?y=1#z')).toBe('http://docs.example.com/x?y=1#z')
    for (const bad of [
      undefined,
      '',
      'javascript:alert(1)',
      'JavaScript:alert(1)',
      'data:text/html,<script>alert(1)</script>',
      'vbscript:msgbox',
      'ftp://docs.example.com/x',
      'file:///etc/passwd',
      'mailto:a@b',
      'blob:https://docs.example.com/x',
      '/docs/mistral',
      'docs.example.com/mistral',
      '//docs.example.com/mistral',
      'https://',
    ]) {
      expect(helpHref(bad), String(bad)).toBeUndefined()
    }
  })
})
