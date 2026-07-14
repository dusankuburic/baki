import {useCallback, useEffect, useRef} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'
import type {ThemeMode} from '@/types'

const TRANSITION_MS = 250

export function useTheme() {
  const theme = useSettingsStore(s => s.settings.appearance.theme)
  const density = useSettingsStore(s => s.settings.appearance.density)
  const reduceMotion = useSettingsStore(s => s.settings.appearance.reduceMotion)
  const highContrast = useSettingsStore(s => s.settings.appearance.highContrast)
  const resolvedTheme = useUIStore(s => s.resolvedTheme)
  const setResolvedTheme = useUIStore(s => s.setResolvedTheme)
  const updateAppearance = useSettingsStore(s => s.updateAppearance)

  const prevTheme = useRef<ThemeMode | null>(null)

  useEffect(() => {
    const resolved = resolveTheme(theme)
    const root = document.documentElement
    const isThemeChange = prevTheme.current !== null && prevTheme.current !== theme
    if (isThemeChange) {
      root.classList.add('theme-transitioning')
    }
    prevTheme.current = theme
    root.dataset.theme = resolved
    if (resolved !== 'system') setResolvedTheme(resolved)
    try {
      localStorage.setItem('pad-theme', theme)
    } catch {
      /* localStorage unavailable */
    }
    if (!isThemeChange) return
    const t = window.setTimeout(() => root.classList.remove('theme-transitioning'), TRANSITION_MS)
    return () => window.clearTimeout(t)
  }, [theme, setResolvedTheme])

  useEffect(() => {
    if (theme !== 'system') return
    const mq = window.matchMedia('(prefers-color-scheme: light)')
    let transitionTimer: number | undefined
    const handler = () => {
      const resolved = mq.matches ? 'light' : 'dark'
      const root = document.documentElement
      root.classList.add('theme-transitioning')
      root.dataset.theme = resolved
      setResolvedTheme(resolved)
      window.clearTimeout(transitionTimer)
      transitionTimer = window.setTimeout(() => root.classList.remove('theme-transitioning'), TRANSITION_MS)
    }
    mq.addEventListener('change', handler)
    return () => {
      mq.removeEventListener('change', handler)
      window.clearTimeout(transitionTimer)
      document.documentElement.classList.remove('theme-transitioning')
    }
  }, [theme, setResolvedTheme])

  useEffect(() => {
    document.documentElement.dataset.density = density
  }, [density])

  useEffect(() => {
    document.documentElement.dataset.reduceMotion = reduceMotion ? 'true' : 'false'
  }, [reduceMotion])

  useEffect(() => {
    document.documentElement.dataset.highContrast = highContrast ? 'true' : 'false'
  }, [highContrast])

  // Sync <meta name="theme-color"> with the active theme's surface-0 so the
  // mobile browser address bar / PWA title bar matches the app background.
  useEffect(() => {
    const surface = getComputedStyle(document.documentElement).getPropertyValue('--surface-0').trim()
    const meta = document.querySelector('meta[name="theme-color"]')
    if (meta && surface) meta.setAttribute('content', surface)
  }, [resolvedTheme])

  const toggleTheme = useCallback(() => {
    const next: ThemeMode = resolvedTheme === 'light' ? 'dark' : 'light'
    updateAppearance({theme: next})
  }, [resolvedTheme, updateAppearance])

  return {theme, resolvedTheme, toggleTheme}
}

function resolveTheme(theme: ThemeMode): ThemeMode {
  if (theme === 'system') return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
  return theme
}
