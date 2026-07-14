import clsx from 'clsx'

type DividerProps = {
  orientation?: 'horizontal' | 'vertical'
  className?: string
}

export default function Divider({orientation = 'horizontal', className}: DividerProps) {
  return (
    <div
      className={clsx(
        'bg-border-subtle flex-shrink-0',
        orientation === 'horizontal' ? 'h-px w-full' : 'w-px h-full',
        className,
      )}
      role="separator"
      aria-orientation={orientation}
    />
  )
}
