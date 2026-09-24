import {useTranslation} from 'react-i18next'
import {createContext, useContext, useMemo, useRef, useCallback, useEffect, useLayoutEffect, ReactNode} from 'react'
import {useUIStore} from '@/stores/uiStore'
import {useIsDesktop} from '@/hooks/useMediaQuery'
import {useDialogFocus} from '@/hooks/useDialogFocus'

// ChatPanelShell hosts the AI chat inside the inspector and, in focus mode,
// lifts the SAME element tree into a full-window reading layout. AITab reaches
// the toggle through this context; it is nullable so AITab still renders
// standalone (in tests, and anywhere the shell isn't used).
interface ChatPanelApi {
  isFocused: boolean
  // Focus mode is desktop-only: below md the inspector is a transformed
  // drawer, which would trap a fixed overlay inside its 320px.
  canFocus: boolean
  toggleFocus: () => void
}
const ChatPanelContext = createContext<ChatPanelApi | null>(null)
export function useChatPanel(): ChatPanelApi | null {
  return useContext(ChatPanelContext)
}

interface Props {
  children: ReactNode
}

export default function ChatPanelShell({children}: Props) {
  const {t} = useTranslation('chat')
  const chatFocus = useUIStore(s => s.chatFocus)
  const isDesktop = useIsDesktop()
  const isFocused = chatFocus && isDesktop
  const dialogRef = useRef<HTMLDivElement>(null)

  // Where focus lands on entering: an element that already has it inside the
  // panel (the header button that was clicked, the composer the shortcut fired
  // from), else the composer, else useDialogFocus's first control. Resolved in
  // a layout effect so it is ready before the hook's deferred focus runs.
  const initialFocusRef = useRef<HTMLElement | null>(null)
  useLayoutEffect(() => {
    if (!isFocused) return
    const active = document.activeElement as HTMLElement | null
    const inside = active && dialogRef.current?.contains(active) ? active : null
    initialFocusRef.current = inside ?? dialogRef.current?.querySelector<HTMLElement>('[data-chat-composer]') ?? null
  }, [isFocused])

  const toggleFocus = useCallback(() => useUIStore.getState().toggleChatFocus(), [])
  const exitFocus = useCallback(() => useUIStore.getState().setChatFocus(false), [])

  // Tab trap and focus restore only. Escape belongs to AITab's
  // `ai.cancel.stream` handler: it already stops a reply first and never sees
  // keys from the portaled dialogs (Compose editor, preview, confirm), whereas
  // a document-level Escape here would also fire for the one that closes them.
  useDialogFocus({isOpen: isFocused, onClose: exitFocus, closeOnEsc: false, containerRef: dialogRef, initialFocusRef})

  // Leaving focus mode keeps focus where the user was working. The hook
  // restores it to whatever opened the overlay (the header toggle, or an
  // element outside), but the panel is the same tree in both modes, so the
  // composer or message the user was in is still there. Captured in the
  // layout phase, before the hook's cleanup moves focus, and re-applied in
  // the passive phase, after it.
  const wasFocusedRef = useRef(false)
  const leavingFromRef = useRef<HTMLElement | null>(null)
  useLayoutEffect(() => {
    if (wasFocusedRef.current && !isFocused) {
      const active = document.activeElement as HTMLElement | null
      leavingFromRef.current = active && dialogRef.current?.contains(active) ? active : null
    }
    wasFocusedRef.current = isFocused
  }, [isFocused])
  useEffect(() => {
    if (isFocused) return
    const el = leavingFromRef.current
    leavingFromRef.current = null
    if (el && el.isConnected) el.focus()
  }, [isFocused])

  const api = useMemo<ChatPanelApi>(
    () => ({isFocused, canFocus: isDesktop, toggleFocus}),
    [isFocused, isDesktop, toggleFocus],
  )

  // ONE element tree for both modes: only classes and ARIA attributes switch.
  // A differently shaped tree per mode remounted the children on every toggle,
  // tearing down the chat (scroll position, search, composer draft) mid-reply.
  // `animate-fade-in` is opacity-only, so the fixed Dropdown/Tooltip inside
  // keep the viewport as their containing block.
  return (
    <div
      className={
        isFocused
          ? 'chat-focus fixed inset-0 z-modal flex flex-col p-3 md:p-6'
          : 'relative flex flex-col h-full min-h-0'
      }
    >
      <button
        type="button"
        tabIndex={-1}
        onClick={exitFocus}
        aria-label={t('panel.exitFocus')}
        className={isFocused ? 'absolute inset-0 bg-surface-overlay animate-fade-in cursor-default' : 'hidden'}
      />
      <div
        ref={dialogRef}
        role={isFocused ? 'dialog' : undefined}
        aria-modal={isFocused || undefined}
        aria-label={isFocused ? t('panel.title') : undefined}
        className={
          isFocused
            ? 'relative flex-1 min-h-0 flex flex-col overflow-hidden w-full max-w-[1400px] mx-auto bg-surface-1 border border-border-subtle rounded-xl shadow-xl animate-fade-in'
            : 'flex-1 min-h-0 flex flex-col overflow-hidden'
        }
      >
        <ChatPanelContext.Provider value={api}>{children}</ChatPanelContext.Provider>
      </div>
    </div>
  )
}
