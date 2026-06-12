import { useEffect, useState, useCallback } from 'react'
import { Search, RefreshCw } from 'lucide-react'
import { libraryApi, type LibraryFlow, type LibraryFilter } from '@/api/library'
import FlowCard from './FlowCard'
import Input from '@/components/shared/Input'
import Button from '@/components/shared/Button'
import Spinner from '@/components/shared/Spinner'

interface LibraryBrowserProps {
  orgId?: string
  onFlowOpen?: (flow: LibraryFlow) => void
  className?: string
}

export default function LibraryBrowser({ orgId, onFlowOpen, className }: LibraryBrowserProps) {
  const [flows, setFlows] = useState<LibraryFlow[]>([])
  const [query, setQuery] = useState('')
  const [isLoading, setIsLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async (filter: LibraryFilter) => {
    setIsLoading(true)
    setError(null)
    try {
      const result = await libraryApi.list(filter)
      setFlows(result)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setIsLoading(false)
    }
  }, [])

  useEffect(() => {
    load({ orgId, limit: 50 })
  }, [orgId, load])

  function handleSearch(e: React.FormEvent) {
    e.preventDefault()
    load({ orgId, query: query.trim() || undefined, limit: 50 })
  }

  return (
    <div className={className}>
      <div className="flex items-center gap-2 mb-4">
        <form onSubmit={handleSearch} className="flex-1 flex gap-2">
          <Input
            icon={Search}
            placeholder="Search flows…"
            value={query}
            onChange={e => setQuery(e.target.value)}
            className="flex-1"
          />
          <Button type="submit" variant="secondary" disabled={isLoading}>Search</Button>
        </form>
        <Button
          variant="ghost"
          icon={RefreshCw}
          onClick={() => load({ orgId, query: query.trim() || undefined, limit: 50 })}
          disabled={isLoading}
          aria-label="Refresh"
        >
          {null}
        </Button>
      </div>

      {error && (
        <div className="mb-4 px-3 py-2 text-sm bg-semantic-error/10 border border-semantic-error/30 rounded-lg text-semantic-error">
          {error}
        </div>
      )}

      {isLoading && flows.length === 0 ? (
        <div className="flex justify-center py-12">
          <Spinner size={28} />
        </div>
      ) : flows.length === 0 ? (
        <div className="flex flex-col items-center justify-center py-12 text-text-muted">
          <p className="text-sm">No flows found</p>
        </div>
      ) : (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3">
          {flows.map(flow => (
            <FlowCard key={flow.id} flow={flow} onOpen={onFlowOpen} />
          ))}
        </div>
      )}
    </div>
  )
}
