import {useEffect} from 'react'
import {flowApi, exportApi} from '@/api'
import {logger} from '@/lib/logger'
import {isTauri} from '@/platform/guards'
import {useUIStore} from '@/stores/uiStore'
import {useEditorStore} from '@/stores/editorStore'
import type {FlowDocument as DomainFlowDocument} from '@/types/domain'

interface Options {
  /** Open a freshly loaded document (handles view switching). */
  openDocument: (doc: DomainFlowDocument | null) => void
  /** Toggle the colour theme (from useTheme). */
  toggleTheme: () => void
  /** Show the keyboard-shortcuts help dialog. */
  onShowShortcuts: () => void
}

/**
 * Wires the Tauri native menu ("menu-event") and OS file-open ("open-file")
 * events to app actions. No-op in web mode. Keeping this here isolates the
 * desktop-only platform wiring out of the App component.
 */
export function useTauriMenuEvents({openDocument, toggleTheme, onShowShortcuts}: Options): void {
  // Native menu bar events.
  useEffect(() => {
    if (!isTauri()) return
    // Sync cancel flag so cleanup is immediate — React does not await async cleanup
    // functions, meaning the Promise.then pattern leaks the listener across re-runs.
    let unsub: (() => void) | null = null
    let cancelled = false
    import('@tauri-apps/api/event').then(({listen}) =>
      listen<string>('menu-event', async (event) => {
        const id = event.payload
        switch (id) {
          case 'file.open': {
            const doc = await flowApi.openFlowFile()
            if (doc) openDocument(doc)
            break
          }
          case 'file.open.folder': {
            const doc = await flowApi.openFlowFolder()
            if (doc) openDocument(doc)
            break
          }
          case 'file.export.pdf':
            exportApi.exportPDF().catch((err) => { logger.warn('PDF export failed', err) })
            break
          case 'file.export.md':
            exportApi.exportMarkdown().catch((err) => { logger.warn('Markdown export failed', err) })
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
      })
    ).then(fn => { if (!cancelled) unsub = fn; else fn() })
    return () => { cancelled = true; unsub?.() }
  }, [openDocument, toggleTheme, onShowShortcuts])

  // OS "open with" / file-association events.
  useEffect(() => {
    if (!isTauri()) return
    let unsub: (() => void) | null = null
    let cancelled = false
    import('@tauri-apps/api/event').then(({listen}) =>
      listen<string[]>('open-file', async (event) => {
        const args = event.payload
        const path = args.find(arg =>
          (arg.toLowerCase().endsWith('.txt') || arg.toLowerCase().endsWith('.pad')) &&
          !arg.toLowerCase().endsWith('.exe')
        )
        if (path) {
          const doc = await flowApi.loadFlowFromPath(path)
          if (doc) openDocument(doc)
        }
      })
    ).then(fn => { if (!cancelled) unsub = fn; else fn() })
    return () => { cancelled = true; unsub?.() }
  }, [openDocument])
}
