import clsx from 'clsx'

type TextareaProps = React.TextareaHTMLAttributes<HTMLTextAreaElement> & {
    error?: string
    hint?: string
}

export default function Textarea({error, hint, className, ...rest}: TextareaProps) {
    return (
        <div className={clsx('flex flex-col', className)}>
            <textarea
                className={clsx(
                    'bg-surface-2 border rounded-md px-3 py-2 text-sm outline-none resize-y min-h-[80px] transition-colors duration-fast',
                    'text-text-primary placeholder:text-text-disabled',
                    error
                        ? 'border-semantic-error focus:border-semantic-error focus:ring-2 focus:ring-semantic-error/20'
                        : 'border-border-default focus:border-focus focus:ring-2 focus:ring-brand-500/20'
                )}
                {...rest}
            />
            {(hint || error) && (
                <span className={clsx('mt-1 text-xs', error ? 'text-semantic-error' : 'text-text-tertiary')}>
                    {error || hint}
                </span>
            )}
        </div>
    )
}
