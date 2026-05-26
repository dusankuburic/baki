import {Zap} from 'lucide-react'

interface Props {
  remaining: number
  limit: number
}

export default function DemoModeBanner({remaining, limit}: Props) {
  return (
    <div className="flex items-center gap-2 px-3 py-1.5 mx-3 rounded-lg bg-brand-500/10 border border-brand-500/20">
      <Zap size={12} className="text-brand-400" />
      <span className="text-xs text-brand-300">
        Demo mode: {remaining}/{limit} remaining today
      </span>
    </div>
  )
}
