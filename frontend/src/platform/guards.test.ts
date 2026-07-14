import {describe, it, expect, beforeEach, afterEach} from 'vitest'
import {simulateTauri, simulateWeb} from '@/testing/testHelpers'
import {isTauri, isWeb, getPlatformType, getPlatformCapabilities} from './guards'

describe('Platform guards', () => {
  afterEach(() => simulateWeb())

  // ---- isTauri / isWeb ----

  describe('isTauri', () => {
    it('returns false in a plain browser environment', () => {
      simulateWeb()
      expect(isTauri()).toBe(false)
    })

    it('returns true when __TAURI__ is present on window', () => {
      const cleanup = simulateTauri()
      expect(isTauri()).toBe(true)
      cleanup()
    })
  })

  describe('isWeb', () => {
    it('returns true in a plain browser environment', () => {
      simulateWeb()
      expect(isWeb()).toBe(true)
    })

    it('returns false when running in Tauri', () => {
      const cleanup = simulateTauri()
      expect(isWeb()).toBe(false)
      cleanup()
    })
  })

  // ---- getPlatformType ----

  describe('getPlatformType', () => {
    it('returns "web" when not in Tauri', () => {
      simulateWeb()
      expect(getPlatformType()).toBe('web')
    })

    it('returns "tauri" when in Tauri', () => {
      const cleanup = simulateTauri()
      expect(getPlatformType()).toBe('tauri')
      cleanup()
    })
  })

  // ---- getPlatformCapabilities ----

  describe('getPlatformCapabilities — web', () => {
    beforeEach(() => simulateWeb())

    it('has no native file system', () => {
      expect(getPlatformCapabilities().fileSystem).toBe(false)
    })

    it('has no native dialogs', () => {
      expect(getPlatformCapabilities().nativeDialogs).toBe(false)
    })

    it('has no system tray', () => {
      expect(getPlatformCapabilities().systemTray).toBe(false)
    })

    it('has clipboard support', () => {
      expect(getPlatformCapabilities().clipboard).toBe(true)
    })

    it('has no native window chrome', () => {
      expect(getPlatformCapabilities().nativeWindow).toBe(false)
    })
  })

  describe('getPlatformCapabilities — tauri', () => {
    let cleanup: () => void
    beforeEach(() => {
      cleanup = simulateTauri()
    })
    afterEach(() => cleanup())

    it('has native file system', () => {
      expect(getPlatformCapabilities().fileSystem).toBe(true)
    })

    it('has native dialogs', () => {
      expect(getPlatformCapabilities().nativeDialogs).toBe(true)
    })

    it('has system tray', () => {
      expect(getPlatformCapabilities().systemTray).toBe(true)
    })

    it('has native window chrome', () => {
      expect(getPlatformCapabilities().nativeWindow).toBe(true)
    })
  })
})
