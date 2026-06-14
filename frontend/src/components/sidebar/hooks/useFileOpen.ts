import {useState, useCallback, useEffect, useRef} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {useEditorStore} from '@/stores/editorStore'
import {useUIStore, isSystemView} from '@/stores/uiStore'
import {flowApi} from '@/api'
import {logger} from '@/lib/logger'
import {isTauri} from '@/platform/guards'
import {useToast} from '@/components/shared'
import type {RecentFile} from '@/types'

export function useFileOpen() {
    const document = useFlowStore(s => s.document)
    const setDocument = useFlowStore(s => s.setDocument)
    const setFolderFiles = useFlowStore(s => s.setFolderFiles)
    const setSelectedFilePath = useFlowStore(s => s.setSelectedFilePath)
    const openInGroup = useEditorStore(s => s.openInGroup)
    const setMainPaneView = useUIStore(s => s.setMainPaneView)
    const toast = useToast()

    const [recentFiles, setRecentFiles] = useState<RecentFile[]>([])
    const [isLoading, setIsLoading] = useState(false)
    const loadIdRef = useRef(0)

    const checkView = useCallback(() => {
        const view = useUIStore.getState().mainPaneView
        if (isSystemView(view)) {
            setMainPaneView('block')
        }
    }, [setMainPaneView])

    const beginLoad = useCallback(() => {
        loadIdRef.current++
        const myId = loadIdRef.current
        setIsLoading(true)
        return () => myId === loadIdRef.current
    }, [])

    const endLoad = useCallback(() => setIsLoading(false), [])

    useEffect(() => {
        if (!isTauri()) return
        flowApi.recentFiles()
            .then((files: RecentFile[]) => { if (files) setRecentFiles(files) })
            .catch((err) => { logger.warn('Failed to load recent files', err) })
    }, [document])

    const handleOpenFile = useCallback(async () => {
        const isLatest = beginLoad()
        try {
            const doc = await flowApi.openFlowFile()
            if (doc && isLatest()) {
                setDocument(doc)
                setFolderFiles([])
                setSelectedFilePath(doc.filePath)
                checkView()
            }
        } catch (err) {
            logger.warn('Failed to open file:', err)
            toast.error('Failed to open file', {description: err instanceof Error ? err.message : String(err)})
        } finally {
            if (isLatest()) endLoad()
        }
    }, [setDocument, setFolderFiles, setSelectedFilePath, checkView, toast, beginLoad, endLoad])

    const handleOpenFolder = useCallback(async () => {
        const isLatest = beginLoad()
        try {
            const doc = await flowApi.openFlowFolder()
            if (doc && isLatest()) {
                setDocument(doc)
                if (doc.files && doc.files.length > 0) {
                    setFolderFiles(doc.files)
                    setSelectedFilePath(doc.files[0].path)
                }
                checkView()
            }
        } catch (err) {
            logger.warn('Failed to open folder:', err)
            toast.error('Failed to open folder', {description: err instanceof Error ? err.message : String(err)})
        } finally {
            if (isLatest()) endLoad()
        }
    }, [setDocument, setFolderFiles, setSelectedFilePath, checkView, toast, beginLoad, endLoad])

    const handleSelectFolderFile = useCallback(async (path: string) => {
        setSelectedFilePath(path)
        const doc = useFlowStore.getState().document
        if (doc) {
            const fileName = path.split(/[/\\]/).pop() ?? ''
            // Find the subflow whose sourceFile matches to avoid a full reload for folder views
            const sf = doc.subflows.find(s => s.sourceFile === fileName)
            if (sf) {
                openInGroup(sf.id)
                checkView()
                return
            }
        }
        
        if (!isTauri()) {
            toast.error('This action requires the desktop app')
            return
        }

        const isLatest = beginLoad()
        try {
            const newDoc = await flowApi.loadFlowFromPath(path)
            if (newDoc && isLatest()) {
                setDocument(newDoc)
                checkView()
            }
        } catch (err) {
            logger.warn('Failed to load file:', err)
            toast.error('Failed to load file', {description: err instanceof Error ? err.message : String(err)})
        } finally {
            if (isLatest()) endLoad()
        }
    }, [setDocument, setSelectedFilePath, openInGroup, checkView, toast, beginLoad, endLoad])

    const handleLoadRecent = useCallback(async (path: string) => {
        if (!isTauri()) return
        const isLatest = beginLoad()
        try {
            const recent = recentFiles.find(f => f.path === path)
            const doc = recent?.isFolder
                ? await flowApi.loadFlowFolder(path)
                : await flowApi.loadFlowFromPath(path)
            if (doc && isLatest()) {
                setDocument(doc)
                if (doc.isFolder && doc.files) {
                    setFolderFiles(doc.files)
                    setSelectedFilePath(doc.files[0].path)
                } else {
                    setFolderFiles([])
                    setSelectedFilePath(doc.filePath)
                }
                checkView()
            }
        } catch (err) {
            logger.warn('Failed to load recent item:', err)
            toast.error('Failed to load file', {description: err instanceof Error ? err.message : String(err)})
        } finally {
            if (isLatest()) endLoad()
        }
    }, [setDocument, setFolderFiles, setSelectedFilePath, recentFiles, checkView, toast, beginLoad, endLoad])

    const handleRemoveRecent = useCallback(async (path: string) => {
        try {
            await flowApi.removeRecentFile(path)
            setRecentFiles(prev => prev.filter(f => f.path !== path))
        } catch (err) {
            logger.warn('Failed to remove recent file:', err)
        }
    }, [])

    const handleClearRecent = useCallback(async () => {
        try {
            await flowApi.clearRecentFiles()
            setRecentFiles([])
        } catch (err) {
            logger.warn('Failed to clear recent files:', err)
        }
    }, [])

    return {
        recentFiles,
        isLoading,
        handleOpenFile,
        handleOpenFolder,
        handleSelectFolderFile,
        handleLoadRecent,
        handleRemoveRecent,
        handleClearRecent,
    }
}
