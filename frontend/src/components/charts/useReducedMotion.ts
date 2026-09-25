import {useEffect, useState} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'

// Recharts animates in JavaScript, so the CSS reduced-motion rules never reach
// it: charts read the preference themselves and pass it to isAnimationActive.
// Both the OS setting and the app's own "reduce motion" switch count.
const QUERY = '(prefers-reduced-motion: reduce)'

function systemPrefers(): boolean {
  return typeof window !== 'undefined' && typeof window.matchMedia === 'function' && window.matchMedia(QUERY).matches
}

export function useReducedMotion(): boolean {
  const setting = useSettingsStore(s => s.settings.appearance.reduceMotion)
  const [system, setSystem] = useState(systemPrefers)
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return
    const mql = window.matchMedia(QUERY)
    const onChange = (e: MediaQueryListEvent) => setSystem(e.matches)
    mql.addEventListener?.('change', onChange)
    return () => mql.removeEventListener?.('change', onChange)
  }, [])
  return setting || system
}
