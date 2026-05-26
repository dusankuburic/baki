import clsx from 'clsx'

type SkeletonProps = {
    className?: string
    lines?: number
}

export default function Skeleton({className, lines = 1}: SkeletonProps) {
    return (
        <div className={clsx('space-y-2', className)}>
            {Array.from({length: lines}).map((_, i) => (
                <div
                    key={i}
                    className={clsx(
                        'h-4 bg-surface-2 rounded animate-pulse-soft',
                        i === lines - 1 && lines > 1 && 'w-3/4'
                    )}
                />
            ))}
        </div>
    )
}
