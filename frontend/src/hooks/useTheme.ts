import {useCallback, useEffect} from 'react'
import {flushSync} from 'react-dom'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore, type ResolvedTheme} from '@/stores/uiStore'
import type {AppearanceSettings, ThemeMode} from '@/types'

// Mirrors the appearance settings onto <html> (data-theme, data-density,
// data-reduce-motion, data-high-contrast), keeps `pad-theme` in localStorage
// for the pre-React boot script in index.html, follows the OS when the theme
// is "system", and exposes toggleTheme for the shortcut and the desktop menu.
//
// The hook subscribes to the settings store directly rather than through
// React state: App calls it, and a React subscription there re-rendered the
// whole shell on every theme change. A switch is now the attribute write and
// one style recalc; only the components that resolve chart colours re-render,
// through uiStore.resolvedTheme.
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
        // The initial sync is not a switch: nothing to cross-fade from.
        applyTheme(resolveTheme(next.theme), prev !== null)
        try {
          localStorage.setItem('pad-theme', next.theme)
        } catch {
          /* localStorage unavailable */
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

// Applies a resolved theme: the data-theme write, the resolvedTheme store
// update (chart colours are read from the computed style at render time, so
// their consumers must render after the attribute is in place) and the
// <meta name="theme-color"> sync.
//
// `animate` cross-fades the switch through the View Transitions API when the
// platform has it and motion is not reduced. The browser blends two snapshots
// on the compositor: the page itself restyles exactly once, with no
// per-element transitions (a `transition` on every element made the recalc an
// order of magnitude slower and blocked the main thread for seconds on large
// views).
function applyTheme(resolved: ResolvedTheme, animate: boolean) {
  const swap = () => {
    document.documentElement.dataset.theme = resolved
    useUIStore.getState().setResolvedTheme(resolved)
    syncThemeColorMeta()
  }
  if (animate && !motionReduced() && typeof document.startViewTransition === 'function') {
    document.startViewTransition(() => {
      // The new snapshot is taken right after this callback: render the
      // chart consumers now rather than on React's next flush.
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

// Keeps the mobile browser address bar / PWA title bar on the active theme's
// surface-0 (index.html does the same before React mounts).
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
