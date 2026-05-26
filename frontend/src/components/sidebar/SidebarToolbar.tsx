import {Sparkles} from 'lucide-react'
import Button from '@/components/shared/Button'

type SidebarToolbarProps = {
    hasFlow: boolean
    findingCount?: number
    onAnalyze: () => void
}

export default function SidebarToolbar({hasFlow, findingCount, onAnalyze}: SidebarToolbarProps) {
    if (!hasFlow) return null

    return (
        <div className="flex items-center h-11 px-3 border-t border-border-subtle">
            <Button
                variant="primary"
                size="sm"
                fullWidth
                icon={Sparkles}
                onClick={onAnalyze}
            >
                Analyze flow
                {findingCount !== undefined && findingCount > 0 && (
                    <span className="ml-1 text-xs opacity-80">{findingCount} findings</span>
                )}
            </Button>
        </div>
    )
}
