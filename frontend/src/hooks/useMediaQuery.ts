import {useState, useEffect} from 'react'

// useMediaQuery subscribes to a CSS media query and re-renders when it changes.
// Used for responsive layout decisions (e.g. collapsing the 3-pane desktop shell
// into a single-pane mobile view with drawer overlays below the md breakpoint).
//
// SSR-safe: returns false on the server / before mount (window is undefined).
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => {
    if (typeof window === 'undefined') return false
    return window.matchMedia(query).matches
  })

  useEffect(() => {
    if (typeof window === 'undefined') return
    const mql = window.matchMedia(query)
    const handler = (e: MediaQueryListEvent) => setMatches(e.matches)
    // Some jsdom versions don't support addEventListener; fall back to
    // addListener (deprecated but still present in older Safari/jest envs).
    if (mql.addEventListener) {
      mql.addEventListener('change', handler)
      return () => mql.removeEventListener('change', handler)
    }
    mql.addListener(handler)
    return () => mql.removeListener(handler)
  }, [query])

  return matches
}

// useIsDesktop is a convenience hook: true when the viewport is ≥ md (768px),
// the breakpoint below which the shell collapses to mobile drawers.
export function useIsDesktop(): boolean {
  return useMediaQuery('(min-width: 768px)')
}
