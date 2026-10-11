import {useTranslation} from 'react-i18next'
import {useRef, useState, type ReactNode} from 'react'
import {useDialogFocus} from '@/hooks/useDialogFocus'

export default function MobileDrawer({
  side,
  label,
  onClose,
  children,
}: {
  side: 'left' | 'right'
  label: string
  onClose: () => void
  children: ReactNode
}) {
  const {t} = useTranslation('shell')
  const ref = useRef<HTMLDivElement>(null)
  useDialogFocus({isOpen: true, onClose, closeOnEsc: true, containerRef: ref})

  const [dragX, setDragX] = useState(0)
  const drag = useRef<{startX: number; width: number} | null>(null)
  const closesLeftward = side === 'left'

  return (
    <div className={`fixed inset-0 z-50 flex ${side === 'right' ? 'justify-end' : ''} md:hidden`}>
      <button
        className="absolute inset-0 bg-surface-overlay/60 backdrop-blur-xs"
        onClick={onClose}
        aria-label={t('drawer.close', {label})}
      />
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-label={label}
        style={{transform: `translateX(${dragX}px)`, touchAction: 'pan-y'}}
        onPointerDown={e => {
          drag.current = {startX: e.clientX, width: ref.current?.offsetWidth ?? 280}
          ;(e.target as HTMLElement).setPointerCapture?.(e.pointerId)
        }}
        onPointerMove={e => {
          if (!drag.current) return
          const dx = e.clientX - drag.current.startX
          const outward = closesLeftward ? Math.min(0, dx) : Math.max(0, dx)
          setDragX(outward)
        }}
        onPointerUp={() => {
          const d = drag.current
          drag.current = null
          if (!d) return
          if (Math.abs(dragX) > d.width * 0.4 || Math.abs(dragX) > 80) {
            onClose()
            setDragX(0)
          } else {
            setDragX(0)
          }
        }}
        className={`relative ${side === 'right' ? 'w-80 border-l' : 'w-72 border-r'} max-w-[80vw] bg-surface-1 border-border-subtle overflow-hidden`}
      >
        {children}
      </div>
    </div>
  )
}
