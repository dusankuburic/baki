import {describe, it, expect} from 'vitest'
import {isImeKeydown} from './imeKey'

describe('isImeKeydown', () => {
  it('flags a keydown fired while an IME composition is active', () => {
    expect(isImeKeydown({isComposing: true})).toBe(true)
    expect(isImeKeydown({nativeEvent: {isComposing: true}})).toBe(true)
  })

  it('flags the Enter WebKit delivers after compositionend (keyCode 229)', () => {
    expect(isImeKeydown({keyCode: 229, isComposing: false})).toBe(true)
    expect(isImeKeydown({nativeEvent: {keyCode: 229, isComposing: false}})).toBe(true)
  })

  it('lets an ordinary key through', () => {
    expect(isImeKeydown({keyCode: 13, isComposing: false})).toBe(false)
    expect(isImeKeydown({})).toBe(false)
  })
})
