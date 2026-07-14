import clsx from 'clsx'

type KbdProps = {
  keys: string[]
  size?: 'xs' | 'sm'
  className?: string
}

const isMac = typeof navigator !== 'undefined' && /Mac|iPod|iPhone|iPad/.test(navigator.userAgent)

const keyMap: Record<string, string> = {
  Cmd: isMac ? '⌘' : 'Ctrl',
  Ctrl: isMac ? '⌃' : 'Ctrl',
  Shift: isMac ? '⇧' : 'Shift',
  Alt: isMac ? '⌥' : 'Alt',
  Enter: isMac ? '↵' : 'Enter',
  Backspace: isMac ? '⌫' : 'Backspace',
  Tab: isMac ? '⇥' : 'Tab',
  Escape: 'Esc',
  Delete: isMac ? '⌦' : 'Del',
}

export default function Kbd({keys, size = 'xs', className}: KbdProps) {
  return (
    <span className={clsx('inline-flex items-center gap-0.5', className)}>
      {keys.map((key, i) => (
        <kbd
          key={i}
          className={clsx(
            'bg-surface-3 border border-border-strong text-text-secondary font-mono rounded',
            size === 'xs' ? 'text-2xs px-1.5 py-0.5' : 'text-xs px-1.5 py-0.5',
          )}
        >
          {keyMap[key] || key}
        </kbd>
      ))}
    </span>
  )
}
