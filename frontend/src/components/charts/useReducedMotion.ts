import {useEffect, useState} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'

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
