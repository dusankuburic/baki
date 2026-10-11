import {useEffect} from 'react'
import {flowApi, exportApi} from '@/api'
import {logger} from '@/lib/logger'
import {getPlatformCapabilities} from '@/platform/guards'
import {useUIStore} from '@/stores/uiStore'
import {useEditorStore} from '@/stores/editorStore'
import type {FlowDocument as DomainFlowDocument} from '@/types'

interface Options {
  openDocument: (doc: DomainFlowDocument | null) => void
  toggleTheme: () => void
  onShowShortcuts: () => void
}

export function useTauriMenuEvents({openDocument, toggleTheme, onShowShortcuts}: Options): void {
  useEffect(() => {
    if (!getPlatformCapabilities().nativeWindow) return
    let unsub: (() => void) | null = null
    let cancelled = false
    void import('@tauri-apps/api/event')
      .then(({listen}) =>
        listen<string>('menu-event', async event => {
          const id = event.payload
          switch (id) {
            case 'file.open': {
              try {
                const doc = await flowApi.openFlowFile()
                if (doc) openDocument(doc)
              } catch (e) {
                logger.warn('Failed to open file:', e)
              }
              break
            }
            case 'file.open.folder': {
              try {
                const doc = await flowApi.openFlowFolder()
                if (doc) openDocument(doc)
              } catch (e) {
                logger.warn('Failed to open folder:', e)
              }
              break
            }
            case 'file.export.pdf':
              exportApi.exportPDF().catch(err => {
                logger.warn('PDF export failed', err)
              })
              break
            case 'file.export.md':
              exportApi.exportMarkdown().catch(err => {
                logger.warn('Markdown export failed', err)
              })
              break
            case 'file.close.tab': {
              const {focusedGroupIndex, groups, closeTab} = useEditorStore.getState()
              const group = groups[focusedGroupIndex]
              if (group?.activeTabId) closeTab(focusedGroupIndex, group.activeTabId)
              break
            }
            case 'view.toggle.sidebar':
              useUIStore.getState().toggleSidebar()
              break
            case 'view.toggle.inspector':
              useUIStore.getState().toggleInspector()
              break
            case 'view.toggle.mode': {
              const current = useUIStore.getState().mainPaneView
              useUIStore.getState().setMainPaneView(current === 'block' ? 'graph' : 'block')
              break
            }
            case 'view.theme.toggle':
              toggleTheme()
              break
            case 'window.reload':
              window.location.reload()
              break
            case 'help.shortcuts':
              onShowShortcuts()
              break
          }
        }),
      )
      .then(fn => {
        if (!cancelled) unsub = fn
        else fn()
      })
    return () => {
      cancelled = true
      unsub?.()
    }
  }, [openDocument, toggleTheme, onShowShortcuts])

  useEffect(() => {
    if (!getPlatformCapabilities().nativeWindow) return
    let unsub: (() => void) | null = null
    let cancelled = false
    void import('@tauri-apps/api/event')
      .then(({listen}) =>
        listen<string[]>('open-file', async event => {
          const args = event.payload
          const path = args.find(
            arg =>
              (arg.toLowerCase().endsWith('.txt') || arg.toLowerCase().endsWith('.pad')) &&
              !arg.toLowerCase().endsWith('.exe'),
          )
          if (path) {
            try {
              const doc = await flowApi.loadFlowFromPath(path)
              if (doc) openDocument(doc)
            } catch (e) {
              logger.warn('Failed to open file from OS event:', e)
            }
          }
        }),
      )
      .then(fn => {
        if (!cancelled) unsub = fn
        else fn()
      })
    return () => {
      cancelled = true
      unsub?.()
    }
  }, [openDocument])
}
