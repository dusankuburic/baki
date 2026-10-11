import {useTranslation} from 'react-i18next'
import {createContext, useContext, useMemo, useRef, useCallback, useEffect, useLayoutEffect, ReactNode} from 'react'
import {useUIStore} from '@/stores/uiStore'
import {useIsDesktop} from '@/hooks/useMediaQuery'
import {useDialogFocus} from '@/hooks/useDialogFocus'

interface ChatPanelApi {
  isFocused: boolean
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

  const initialFocusRef = useRef<HTMLElement | null>(null)
  useLayoutEffect(() => {
    if (!isFocused) return
    const active = document.activeElement as HTMLElement | null
    const inside = active && dialogRef.current?.contains(active) ? active : null
    initialFocusRef.current = inside ?? dialogRef.current?.querySelector<HTMLElement>('[data-chat-composer]') ?? null
  }, [isFocused])

  const toggleFocus = useCallback(() => useUIStore.getState().toggleChatFocus(), [])
  const exitFocus = useCallback(() => useUIStore.getState().setChatFocus(false), [])

  useDialogFocus({isOpen: isFocused, onClose: exitFocus, closeOnEsc: false, containerRef: dialogRef, initialFocusRef})

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
