import {describe, it, expect, beforeEach} from 'vitest'
import {clearPostLogoutNotice, readPostLogoutNotice, setPostLogoutNotice} from './authNotice'

beforeEach(() => sessionStorage.clear())

describe('post-logout notice', () => {
  it('round-trips through sessionStorage and clears on demand', () => {
    expect(readPostLogoutNotice()).toBeNull()
    setPostLogoutNotice('password-changed')
    expect(readPostLogoutNotice()).toBe('password-changed')
    clearPostLogoutNotice()
    expect(readPostLogoutNotice()).toBeNull()
  })

  it('ignores a value it did not write', () => {
    sessionStorage.setItem('auth_notice', 'something-else')
    expect(readPostLogoutNotice()).toBeNull()
  })
})
