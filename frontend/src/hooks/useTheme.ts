import {useCallback, useEffect} from 'react'
import {flushSync} from 'react-dom'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore, type ResolvedTheme} from '@/stores/uiStore'
import type {AppearanceSettings, ThemeMode} from '@/types'

export function useTheme() {
  useEffect(() => {
    const root = document.documentElement
    let current: AppearanceSettings | null = null
    let system: {mq: MediaQueryList; onChange: () => void} | null = null

    const followSystem = (on: boolean) => {
      if (system) {
        system.mq.removeEventListener('change', system.onChange)
        system = null
      }
      if (!on) return
      const mq = window.matchMedia('(prefers-color-scheme: light)')
      const onChange = () => applyTheme(mq.matches ? 'light' : 'dark', true)
      mq.addEventListener('change', onChange)
      system = {mq, onChange}
    }

    const sync = (next: AppearanceSettings) => {
      const prev = current
      current = next
      if (!prev || prev.theme !== next.theme) {
        applyTheme(resolveTheme(next.theme), prev !== null)
        try {
          localStorage.setItem('pad-theme', next.theme)
        } catch {
        }
        followSystem(next.theme === 'system')
      }
      if (!prev || prev.density !== next.density) root.dataset.density = next.density
      if (!prev || prev.reduceMotion !== next.reduceMotion) {
        root.dataset.reduceMotion = next.reduceMotion ? 'true' : 'false'
      }
      if (!prev || prev.highContrast !== next.highContrast) {
        root.dataset.highContrast = next.highContrast ? 'true' : 'false'
      }
    }

    sync(useSettingsStore.getState().settings.appearance)
    const unsubscribe = useSettingsStore.subscribe(state => sync(state.settings.appearance))
    return () => {
      unsubscribe()
      followSystem(false)
    }
  }, [])

  const toggleTheme = useCallback(() => {
    const next: ThemeMode = useUIStore.getState().resolvedTheme === 'light' ? 'dark' : 'light'
    void useSettingsStore.getState().updateAppearance({theme: next})
  }, [])

  return {toggleTheme}
}

function applyTheme(resolved: ResolvedTheme, animate: boolean) {
  const swap = () => {
    document.documentElement.dataset.theme = resolved
    useUIStore.getState().setResolvedTheme(resolved)
    syncThemeColorMeta()
  }
  if (animate && !motionReduced() && typeof document.startViewTransition === 'function') {
    document.startViewTransition(() => {
      flushSync(swap)
    })
    return
  }
  swap()
}

function motionReduced(): boolean {
  if (useSettingsStore.getState().settings.appearance.reduceMotion) return true
  return typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches
}

function syncThemeColorMeta() {
  const meta = document.querySelector('meta[name="theme-color"]')
  if (!meta) return
  const surface = getComputedStyle(document.documentElement).getPropertyValue('--surface-0').trim()
  if (surface) meta.setAttribute('content', surface)
}

function resolveTheme(theme: ThemeMode): ResolvedTheme {
  if (theme === 'system') return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
  return theme
}
