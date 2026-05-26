import {useState, useCallback, useEffect} from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {flowApi} from '@/api'
import type {RecentFile, FlowDocument as DomainFlowDocument, FlowFileInfo} from '@/types/domain'

export function useFileOpen() {
    const document = useFlowStore(s => s.document)
    const setDocument = useFlowStore(s => s.setDocument)
    const setFolderFiles = useFlowStore(s => s.setFolderFiles)
    const setSelectedFilePath = useFlowStore(s => s.setSelectedFilePath)
    const openInGroup = useFlowStore(s => s.openInGroup)

    const [recentFiles, setRecentFiles] = useState<RecentFile[]>([])

    useEffect(() => {
        flowApi.recentFiles()
            .then((files: RecentFile[]) => { if (files) setRecentFiles(files) })
            .catch(() => {})
    }, [document])

    const handleOpenFile = useCallback(async () => {
        try {
            const doc = await flowApi.openFlowFile()
            if (doc) {
                const domainDoc = doc as any as DomainFlowDocument
                setDocument(domainDoc)
                setFolderFiles([])
                setSelectedFilePath(domainDoc.filePath)
            }
        } catch (err) {
            console.error('Failed to open file:', err)
        }
    }, [setDocument, setFolderFiles, setSelectedFilePath])

    const handleOpenFolder = useCallback(async () => {
        try {
            const doc = await flowApi.openFlowFolder()
            if (doc) {
                const domainDoc = doc as any as DomainFlowDocument
                setDocument(domainDoc)
                if (domainDoc.files && domainDoc.files.length > 0) {
                    setFolderFiles(domainDoc.files)
                    setSelectedFilePath(domainDoc.files[0].path)
                }
            }
        } catch (err) {
            console.error('Failed to open folder:', err)
        }
    }, [setDocument, setFolderFiles, setSelectedFilePath])

    const handleSelectFolderFile = useCallback(async (path: string) => {
        setSelectedFilePath(path)
        const doc = useFlowStore.getState().document
        if (doc) {
            const fileName = path.split(/[/\\]/).pop() ?? ''
            // Find the subflow whose sourceFile matches to avoid a full reload for folder views
            const sf = doc.subflows.find(s => s.sourceFile === fileName)
            if (sf) {
                openInGroup(sf.id)
                return
            }
        }
        try {
            const newDoc = await flowApi.loadFlowFromPath(path)
            if (newDoc) setDocument(newDoc as any as DomainFlowDocument)
        } catch (err) {
            console.error('Failed to load file:', err)
        }
    }, [setDocument, setSelectedFilePath, openInGroup])

    const handleLoadRecent = useCallback(async (path: string) => {
        try {
            const recent = recentFiles.find(f => f.path === path)
            const doc = recent?.isFolder 
                ? await flowApi.loadFlowFolder(path)
                : await flowApi.loadFlowFromPath(path)
            if (doc) {
                const domainDoc = doc as any as DomainFlowDocument
                setDocument(domainDoc)
                if (domainDoc.isFolder && domainDoc.files) {
                    setFolderFiles(domainDoc.files)
                    setSelectedFilePath(domainDoc.files[0].path)
                } else {
                    setFolderFiles([])
                    setSelectedFilePath(domainDoc.filePath)
                }
            }
        } catch (err) {
            console.error('Failed to load recent item:', err)
        }
    }, [setDocument, setFolderFiles, setSelectedFilePath, recentFiles])

    const handleRemoveRecent = useCallback(async (path: string) => {
        try {
            await flowApi.removeRecentFile(path)
            setRecentFiles(prev => prev.filter(f => f.path !== path))
        } catch (err) {
            console.error('Failed to remove recent file:', err)
        }
    }, [])

    const handleClearRecent = useCallback(async () => {
        try {
            await flowApi.clearRecentFiles()
            setRecentFiles([])
        } catch (err) {
            console.error('Failed to clear recent files:', err)
        }
    }, [])

    return {
        recentFiles,
        handleOpenFile,
        handleOpenFolder,
        handleSelectFolderFile,
        handleLoadRecent,
        handleRemoveRecent,
        handleClearRecent,
    }
}
