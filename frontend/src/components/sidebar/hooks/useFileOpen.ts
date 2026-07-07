import {useState, useCallback, useEffect} from 'react'
import {useFlowStore, beginDocLoad, isDocLoadCurrent} from '@/stores/flowStore'
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

    const checkView = useCallback(() => {
        const view = useUIStore.getState().mainPaneView
        if (isSystemView(view)) {
            setMainPaneView('block')
        }
    }, [setMainPaneView])

    const setIsLoadingState = useCallback(() => setIsLoading(true), [])
    const endLoad = useCallback(() => setIsLoading(false), [])

    useEffect(() => {
        if (!isTauri()) return
        flowApi.recentFiles()
            .then((files: RecentFile[]) => { if (files) setRecentFiles(files) })
            .catch((err) => { logger.warn('Failed to load recent files', err) })
    }, [document?.id])

    const handleOpenFile = useCallback(async () => {
        const gen = beginDocLoad()
        setIsLoadingState()
        try {
            const doc = await flowApi.openFlowFile()
            if (doc && isDocLoadCurrent(gen)) {
                setDocument(doc)
                setFolderFiles([])
                setSelectedFilePath(doc.filePath)
                checkView()
            }
        } catch (err) {
            logger.warn('Failed to open file:', err)
            if (isDocLoadCurrent(gen)) toast.error('Failed to open file', {description: err instanceof Error ? err.message : String(err)})
        } finally {
            if (isDocLoadCurrent(gen)) endLoad()
        }
    }, [setDocument, setFolderFiles, setSelectedFilePath, checkView, toast, setIsLoadingState, endLoad])

    const handleOpenFolder = useCallback(async () => {
        const gen = beginDocLoad()
        setIsLoadingState()
        try {
            const doc = await flowApi.openFlowFolder()
            if (doc && isDocLoadCurrent(gen)) {
                setDocument(doc)
                if (doc.files && doc.files.length > 0) {
                    setFolderFiles(doc.files)
                    setSelectedFilePath(doc.files[0].path)
                }
                checkView()
            }
        } catch (err) {
            logger.warn('Failed to open folder:', err)
            if (isDocLoadCurrent(gen)) toast.error('Failed to open folder', {description: err instanceof Error ? err.message : String(err)})
        } finally {
            if (isDocLoadCurrent(gen)) endLoad()
        }
    }, [setDocument, setFolderFiles, setSelectedFilePath, checkView, toast, setIsLoadingState, endLoad])

    const handleSelectFolderFile = useCallback(async (path: string) => {
        setSelectedFilePath(path)
        const doc = useFlowStore.getState().document
        if (doc) {
            const fileName = path.split(/[/\\]/).pop() ?? ''
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

        const gen = beginDocLoad()
        setIsLoadingState()
        try {
            const newDoc = await flowApi.loadFlowFromPath(path)
            if (newDoc && isDocLoadCurrent(gen)) {
                setDocument(newDoc)
                checkView()
            }
        } catch (err) {
            logger.warn('Failed to load file:', err)
            if (isDocLoadCurrent(gen)) toast.error('Failed to load file', {description: err instanceof Error ? err.message : String(err)})
        } finally {
            if (isDocLoadCurrent(gen)) endLoad()
        }
    }, [setDocument, setSelectedFilePath, openInGroup, checkView, toast, setIsLoadingState, endLoad])

    const handleLoadRecent = useCallback(async (path: string) => {
        if (!isTauri()) return
        const gen = beginDocLoad()
        setIsLoadingState()
        try {
            const recent = recentFiles.find(f => f.path === path)
            const doc = recent?.isFolder
                ? await flowApi.loadFlowFolder(path)
                : await flowApi.loadFlowFromPath(path)
            if (doc && isDocLoadCurrent(gen)) {
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
            if (isDocLoadCurrent(gen)) toast.error('Failed to load file', {description: err instanceof Error ? err.message : String(err)})
        } finally {
            if (isDocLoadCurrent(gen)) endLoad()
        }
    }, [setDocument, setFolderFiles, setSelectedFilePath, recentFiles, checkView, toast, setIsLoadingState, endLoad])

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

    // handleRevealFile opens the OS file manager with the file selected.
    // Requires a live local filesystem, so (like the other path-based actions
    // in this hook) it's desktop-only.
    const handleRevealFile = useCallback(async (path: string) => {
        if (!isTauri()) {
            toast.error('This action requires the desktop app')
            return
        }
        try {
            await flowApi.revealInFileManager(path)
        } catch (err) {
            logger.warn('Failed to reveal file:', err)
            toast.error('Failed to reveal file', {description: err instanceof Error ? err.message : String(err)})
        }
    }, [toast])

    // handleReloadFile force-reloads a subflow file from disk, bypassing the
    // "already open, just switch tabs" shortcut in handleSelectFolderFile —
    // for picking up edits made outside the app.
    const handleReloadFile = useCallback(async (path: string) => {
        if (!isTauri()) {
            toast.error('This action requires the desktop app')
            return
        }
        const gen = beginDocLoad()
        setIsLoadingState()
        try {
            const doc = await flowApi.loadFlowFromPath(path)
            if (doc && isDocLoadCurrent(gen)) {
                setDocument(doc)
                checkView()
                toast.success('Reloaded from disk')
            }
        } catch (err) {
            logger.warn('Failed to reload file:', err)
            if (isDocLoadCurrent(gen)) toast.error('Failed to reload file', {description: err instanceof Error ? err.message : String(err)})
        } finally {
            if (isDocLoadCurrent(gen)) endLoad()
        }
    }, [setDocument, checkView, toast, setIsLoadingState, endLoad])

    return {
        recentFiles,
        isLoading,
        handleOpenFile,
        handleOpenFolder,
        handleSelectFolderFile,
        handleLoadRecent,
        handleRemoveRecent,
        handleClearRecent,
        handleRevealFile,
        handleReloadFile,
    }
}
