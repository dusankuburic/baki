import clsx from 'clsx'

type ConnectionState = 'connected' | 'connecting' | 'disconnected' | 'error'

interface Props {
  state: ConnectionState
  provider?: string
}

export default function ConnectionStatus({state, provider}: Props) {
  const statusLabels = {
    connected: 'Connected',
    connecting: 'Connecting...',
    disconnected: 'Disconnected',
    error: 'Connection Error'
  }

  return (
    <div className="flex items-center gap-1.5 px-2 py-1 rounded-full bg-surface-2 border border-border-subtle">
      <span
        className={clsx(
          'w-1.5 h-1.5 rounded-full',
          state === 'connected' && 'bg-success shadow-[0_0_8px_var(--success)]',
          state === 'connecting' && 'bg-warning animate-pulse-soft',
          state === 'disconnected' && 'bg-text-disabled',
          state === 'error' && 'bg-error animate-pulse-soft shadow-[0_0_8px_var(--error)]'
        )}
      />
      <span className="text-2xs text-text-tertiary">
        {statusLabels[state]}
      </span>
      {provider && state === 'connected' && (
        <span className="text-2xs text-text-tertiary/60">· {provider}</span>
      )}
    </div>
  )
}
