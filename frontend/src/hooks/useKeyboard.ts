import {useEffect, useMemo, useRef} from 'react'
import {shortcuts, matchesShortcut, type ShortcutScope} from '@/lib/shortcuts'

interface UseKeyboardOptions {
  scope?: ShortcutScope | ShortcutScope[]
  handlers: Partial<Record<string, (e: KeyboardEvent) => void>>
  enabled?: boolean
  containerRef?: React.RefObject<HTMLElement | null>
}

export function useKeyboard({scope, handlers, enabled = true, containerRef}: UseKeyboardOptions) {
  const scopes = useMemo(() => (Array.isArray(scope) ? scope : scope ? [scope] : ['global' as ShortcutScope]), [scope])

  const handlersRef = useRef(handlers)
  useEffect(() => {
    handlersRef.current = handlers
  })
  useEffect(() => {
    if (!enabled) return

    function handler(e: KeyboardEvent) {
      if (containerRef?.current) {
        if (!containerRef.current.contains(document.activeElement)) return
      }

      const target = e.target as HTMLElement
      const inInput = target.matches('input, textarea, [contenteditable="true"]')

      for (const s of shortcuts) {
        if (!scopes.includes(s.scope)) continue
        if (inInput && !s.allowInInputs) continue
        if (!matchesShortcut(e, s.keys)) continue

        const fn = handlersRef.current[s.id]
        if (!fn) continue

        if (s.preventDefault !== false) e.preventDefault()
        fn(e)
        return
      }
    }

    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [scopes, enabled, containerRef])
}
