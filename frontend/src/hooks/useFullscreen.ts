import {useCallback, useSyncExternalStore} from 'react'

// useFullscreen mirrors the document's fullscreen state and toggles it.
//
// The toolbar button used to call requestFullscreen on every click, so the
// second click did nothing and the only way back was the browser's Escape.
// The state comes from `fullscreenchange`, so leaving fullscreen by any route
// (this toggle, Escape, the browser's own controls) flips the button back.

function subscribe(onChange: () => void) {
  document.addEventListener('fullscreenchange', onChange)
  return () => document.removeEventListener('fullscreenchange', onChange)
}

function getSnapshot() {
  return !!document.fullscreenElement
}

function getServerSnapshot() {
  return false
}

// requestFullscreen reports refusal (no user activation, fullscreen disabled
// by an embedding iframe) by REJECTING, which a try/catch around a `void`ed
// call never sees — it surfaced as an unhandled rejection. The try covers
// engines where the method is missing and the call throws synchronously.
export function enterFullscreen() {
  try {
    document.documentElement.requestFullscreen().catch(() => {})
  } catch {
    /* fullscreen not supported */
  }
}

export function exitFullscreen() {
  try {
    document.exitFullscreen().catch(() => {})
  } catch {
    /* not fullscreen, or unsupported */
  }
}

export function useFullscreen(): {isFullscreen: boolean; toggleFullscreen: () => void} {
  const isFullscreen = useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot)
  const toggleFullscreen = useCallback(() => {
    if (document.fullscreenElement) exitFullscreen()
    else enterFullscreen()
  }, [])
  return {isFullscreen, toggleFullscreen}
}
