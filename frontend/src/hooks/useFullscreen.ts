import {useCallback, useSyncExternalStore} from 'react'

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

export function enterFullscreen() {
  try {
    document.documentElement.requestFullscreen().catch(() => {})
  } catch {
  }
}

export function exitFullscreen() {
  try {
    document.exitFullscreen().catch(() => {})
  } catch {
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
