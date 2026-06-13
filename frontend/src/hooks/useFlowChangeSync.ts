import {useEffect, useRef} from 'react'
import {onFlowChanged, usePresenceStore} from '@/stores/presenceStore'
import {useFlowStore} from '@/stores/flowStore'
import {libraryApi} from '@/api/library'
import {logger} from '@/lib/logger'
import type {FlowDocument} from '@/types/domain'

export function useFlowChangeSync(): void {
    const flowId = usePresenceStore(s => s.flowId)
    const status = usePresenceStore(s => s.status)
    const lastVersionRef = useRef(0)

    useEffect(() => {
        if (!flowId || status !== 'connected') return

        return onFlowChanged((version: number) => {
            if (version === lastVersionRef.current) return
            lastVersionRef.current = version

            const currentDoc = useFlowStore.getState().document
            if (!currentDoc || currentDoc.id !== flowId) return

            libraryApi.getContent(flowId)
                .then((content) => {
                    useFlowStore.getState().setDocument(content as FlowDocument)
                })
                .catch((err) => {
                    logger.warn('Failed to reload flow after remote change', err)
                })
        })
    }, [flowId, status])
}
