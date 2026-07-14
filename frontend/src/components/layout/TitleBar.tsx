import {Minus, Square, X, Settings} from 'lucide-react'
import {createAdapter} from '@/platform/adapters'
import {getPlatformCapabilities} from '@/platform/guards'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import OrgSwitcher from './OrgSwitcher'
import PresenceIndicators from '@/components/collaboration/PresenceIndicators'

const platform = createAdapter()

export default function TitleBar() {
  const document = useFlowStore(s => s.document)
  const selectedSubflowId = useFlowStore(s => s.selectedSubflowId)

  const subflow = document?.subflows.find(s => s.id === selectedSubflowId)
  const breadcrumb = document ? [document.name, ...(subflow ? [subflow.name] : [])] : ['PAD Analyzer']

  const toggleSettings = useUIStore(s => s.toggleSettings)

  return (
    <div
      className="flex items-center h-8 px-3 border-b border-border-subtle bg-surface-1 flex-shrink-0 print:hidden"
      data-tauri-drag-region
    >
      <span className="text-xs font-medium text-text-tertiary select-none truncate pointer-events-none">
        {breadcrumb.map((segment, i) => (
          <span key={i}>
            {i > 0 && <span className="text-text-tertiary mx-1">›</span>}
            <span className={i === breadcrumb.length - 1 ? 'text-text-secondary' : 'text-text-tertiary'}>
              {segment}
            </span>
          </span>
        ))}
      </span>
      <div className="flex-1 h-full pointer-events-none" />
      <div className="flex items-center gap-1">
        <PresenceIndicators className="mr-1" />
        <OrgSwitcher />
        <button
          onClick={toggleSettings}
          className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast"
          title="Settings (Ctrl+,)"
          aria-label="Settings"
        >
          <Settings size={12} />
        </button>
        {getPlatformCapabilities().nativeWindow && (
          <>
            <button
              onClick={() => platform.minimizeWindow()}
              className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast"
            >
              <Minus size={10} />
            </button>
            <button
              onClick={() => platform.toggleMaximizeWindow()}
              className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-surface-3 text-text-tertiary hover:text-text-secondary transition-colors duration-fast"
            >
              <Square size={8} />
            </button>
            <button
              onClick={() => platform.closeWindow()}
              className="w-6 h-6 flex items-center justify-center rounded-sm hover:bg-semantic-error/15 text-text-tertiary hover:text-semantic-error transition-colors duration-fast"
            >
              <X size={10} />
            </button>
          </>
        )}
      </div>
    </div>
  )
}
