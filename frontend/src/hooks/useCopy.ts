import {useState, useCallback, useRef, useEffect} from 'react'
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

  // Clear the feedback timer on unmount so setCopied(false) can't fire on a
  // gone component (React 18 ignores it, but this keeps the cleanup contract).
  useEffect(() => {
    return () => {
      if (timerRef.current) clearTimeout(timerRef.current)
    }
  }, [])

  const copy = useCallback(
    (text: string) => {
      writeClipboard(text)
        .then(() => {
          setCopied(true)
          if (timerRef.current) clearTimeout(timerRef.current)
          timerRef.current = setTimeout(() => setCopied(false), timeout)
        })
        .catch(() => {})
    },
    [timeout],
  )

  return {copied, copy}
}
