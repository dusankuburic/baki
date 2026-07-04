import {request} from './client'
import type {FlowDiff} from '@/types'
import {createAdapter} from '@/platform/adapters'
import {isTauri} from '@/platform/guards'

// Rendering + base64-encoding a large flow's PDF/Markdown report can take
// longer than the default request timeout.
const EXPORT_TIMEOUT_MS = 90_000

export const exportApi = {
  exportPDF: async (): Promise<string> => {
    let path = ''
    if (isTauri()) {
      const p = await createAdapter().fileSave({
        filters: [{name: 'PDF', extensions: ['pdf']}]
      })
      if (!p) return ''
      path = p
    }

    const res: {data: string} = await request('/api/export/pdf', {path}, 'POST', EXPORT_TIMEOUT_MS)
    
    if (!isTauri() && res.data) {
      const link = document.createElement('a')
      link.href = `data:application/pdf;base64,${res.data}`
      link.download = 'export.pdf'
      document.body.appendChild(link)
      link.click()
      document.body.removeChild(link)
    }
    return path
  },

  exportMarkdown: async (): Promise<string> => {
    let path = ''
    if (isTauri()) {
      const p = await createAdapter().fileSave({
        filters: [{name: 'Markdown', extensions: ['md']}]
      })
      if (!p) return ''
      path = p
    }

    const res: {data: string} = await request('/api/export/markdown', {path}, 'POST', EXPORT_TIMEOUT_MS)
    
    if (!isTauri() && res.data) {
      const link = document.createElement('a')
      link.href = `data:text/markdown;base64,${res.data}`
      link.download = 'export.md'
      document.body.appendChild(link)
      link.click()
      document.body.removeChild(link)
    }
    return path
  },

  compareCurrentWith: (path: string): Promise<FlowDiff> =>
    request('/api/export/compare', {path}),

  pickFile: async (filter: string): Promise<string | null> => {
    const extensions = filter ? filter.split(',') : undefined
    const path = await createAdapter().fileOpen({
      filters: extensions ? [{name: 'Files', extensions}] : undefined
    })
    return Array.isArray(path) ? path[0] : path
  },
}
