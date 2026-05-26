import {useState} from 'react'
import clsx from 'clsx'
import {ChevronRight} from 'lucide-react'

type CollapsibleSectionProps = {
    title: string
    defaultOpen?: boolean
    children: React.ReactNode
}

export function CollapsibleSection({title, defaultOpen = true, children}: CollapsibleSectionProps) {
    const [open, setOpen] = useState(defaultOpen)

    return (
        <div className="border-b border-border-subtle">
            <button
                className="flex items-center justify-between w-full h-9 px-4 text-sm font-medium text-text-secondary hover:text-text-primary"
                onClick={() => setOpen(v => !v)}
            >
                {title}
                <ChevronRight
                    size={14}
                    className={clsx('transition-transform duration-fast', open && 'rotate-90')}
                />
            </button>
            {open && <div className="px-4 pb-3">{children}</div>}
        </div>
    )
}
