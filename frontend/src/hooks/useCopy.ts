import {useState, useCallback, useRef} from 'react'
import {writeClipboard} from '@/lib/clipboard'

/**
 * Copy-to-clipboard hook with "copied" visual-feedback state.
 * Replaces the repeated navigator.clipboard.writeText + setTimeout pattern.
 */
export function useCopy(timeout = 2000): {
  copied: boolean
  copy: (text: string) => void
} {
  const [copied, setCopied] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const copy = useCallback((text: string) => {
    writeClipboard(text).then(() => {
      setCopied(true)
      if (timerRef.current) clearTimeout(timerRef.current)
      timerRef.current = setTimeout(() => setCopied(false), timeout)
    }).catch(() => {})
  }, [timeout])

  return {copied, copy}
}
