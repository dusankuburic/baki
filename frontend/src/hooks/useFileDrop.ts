import {useCallback, useState, useEffect} from 'react'
import {useToast} from '@/components/shared'
import {flowApi} from '@/api'
import {isTauri} from '@/platform/guards'
import {logger} from '@/lib/logger'
import {useSettingsStore} from '@/stores/settingsStore'
import type {FlowDocument as DomainFlowDocument} from '@/types'

export function useFileDrop(openDocument: (doc: DomainFlowDocument | null) => void) {
    const toast = useToast()
    const [dragOver, setDragOver] = useState(false)

    // Tauri v2 windows default to native OS drag-and-drop handling
    // (dragDropEnabled: true in tauri.conf.json, unset here), which
    // intercepts the drop at the OS/webview level — the browser's HTML5
    // dragover/drop events below never fire on desktop. The native
    // equivalent is the webview's onDragDropEvent stream, which this
    // subscribes to only in Tauri; the HTML5 handlers still cover the web
    // build, where no such native API exists.
    useEffect(() => {
        if (!isTauri()) return
        let unlisten: (() => void) | undefined
        let cancelled = false
        import('@tauri-apps/api/webview').then(({getCurrentWebview}) =>
            getCurrentWebview().onDragDropEvent((event) => {
                switch (event.payload.type) {
                    case 'enter':
                    case 'over':
                        setDragOver(true)
                        break
                    case 'leave':
                        setDragOver(false)
                        break
                    case 'drop': {
                        setDragOver(false)
                        const path = event.payload.paths[0]
                        if (!path) break
                        flowApi.loadFlowFromPath(path)
                            .then(doc => { if (doc) openDocument(doc) })
                            .catch(err => {
                                logger.warn('Failed to open dropped file:', err)
                                toast.error('Failed to open file', {description: String(err)})
                            })
                        break
                    }
                }
            })
        ).then(fn => { if (cancelled) fn(); else unlisten = fn })
          .catch(err => logger.warn('Failed to subscribe to native drag-drop events:', err))
        return () => { cancelled = true; unlisten?.() }
    }, [openDocument, toast])

    const handleDragOver = useCallback((e: React.DragEvent) => {
        e.preventDefault()
        e.stopPropagation()
        if (e.dataTransfer.types.includes('Files')) setDragOver(true)
    }, [])

    const handleDragLeave = useCallback((e: React.DragEvent) => {
        e.preventDefault()
        e.stopPropagation()
        setDragOver(false)
    }, [])

    const handleDrop = useCallback(async (e: React.DragEvent) => {
        e.preventDefault()
        e.stopPropagation()
        setDragOver(false)
        const files = e.dataTransfer.files
        if (files.length > 0) {
            const file = files[0]
            if (!file) return
            try {
                if (isTauri()) {
                    const path = (file as File & {path?: string}).path
                    if (path) {
                        const doc = await flowApi.loadFlowFromPath(path)
                        if (doc) openDocument(doc)
                    }
                } else {
                    const maxBytes = useSettingsStore.getState().settings.parser.maxFileSizeMB * 1024 * 1024
                    if (file.size > maxBytes) {
                      toast.error('File too large', {description: `${(file.size / 1024 / 1024).toFixed(1)}MB exceeds the ${useSettingsStore.getState().settings.parser.maxFileSizeMB}MB limit`})
                      return
                    }
                    const content = await new Promise<string>((resolve) => {
                        const reader = new FileReader()
                        reader.onload = (e) => resolve(e.target?.result as string)
                        reader.readAsText(file)
                    })
                    const doc = await flowApi.uploadFlow(file.name, {[file.name]: content})
                    if (doc) openDocument(doc)
                }
            } catch (err) {
                logger.warn('Failed to open dropped file:', err)
                toast.error('Failed to open file', {description: String(err)})
            }
        }
    }, [openDocument, toast])

    return {dragOver, handleDragOver, handleDragLeave, handleDrop}
}
