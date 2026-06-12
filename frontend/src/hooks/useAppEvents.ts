import {useEffect} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {subscribeToEvents} from '@/api/client'
import type {FlowDocument as DomainFlowDocument} from '@/types/domain'

export function useAppEvents(deps: {
    openDocument: (doc: DomainFlowDocument | null) => void
}) {
    const {openDocument} = deps

    useEffect(() => {
        let unsub: (() => void) | null = null
        let cancelled = false
        subscribeToEvents((ev) => {
            const d = ev.data as Record<string, unknown> | undefined
            if (ev.name === 'flow:parse-progress') {
                useFlowStore.setState({parseProgress: (d?.percent as number) ?? 0, isParsing: true})
            } else if (ev.name === 'flow:loaded') {
                if (d) openDocument(d as unknown as DomainFlowDocument)
            } else if (ev.name === 'flow:load-error') {
                useFlowStore.getState().setParseError((d?.error as string) ?? 'Unknown error')
            }
        }).then(fn => { if (!cancelled) unsub = fn; else fn() })
        return () => {
            cancelled = true
            unsub?.()
        }
    }, [openDocument])
}
