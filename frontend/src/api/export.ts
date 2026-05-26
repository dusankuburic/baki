import {request} from './client'
import {open, save} from '@tauri-apps/plugin-dialog'
import type {FlowDiff} from '@/types/domain'

export const exportApi = {
  exportPDF: async (): Promise<string> => {
    const path = await save({
      filters: [{name: 'PDF', extensions: ['pdf']}]
    })
    if (!path) return ''
    return request('/api/export/pdf', {path})
  },

  exportMarkdown: async (): Promise<string> => {
    const path = await save({
      filters: [{name: 'Markdown', extensions: ['md']}]
    })
    if (!path) return ''
    return request('/api/export/markdown', {path})
  },

  compareCurrentWith: (path: string): Promise<FlowDiff> =>
    request('/api/export/compare', {path}),

  pickFile: async (filter: string): Promise<string | null> => {
    const path = await open({
      filters: filter ? [{name: 'Files', extensions: filter.split(',')}] : undefined
    })
    return Array.isArray(path) ? path[0] : path
  },
}
