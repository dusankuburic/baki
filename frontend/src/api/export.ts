import {request} from './client'
import type {FlowDiff} from '@/types'
import {createAdapter} from '@/platform/adapters'
import {getPlatformCapabilities} from '@/platform/guards'
import {useFlowStore} from '@/stores/flowStore'

const EXPORT_TIMEOUT_MS = 90_000

function currentFlowId(): string {
  return useFlowStore.getState().document?.id ?? ''
}

async function exportReport(
  kind: {endpoint: string; mime: string; label: string; ext: string},
  flowId = currentFlowId(),
): Promise<string> {
  let path = ''
  if (getPlatformCapabilities().nativeDialogs) {
    const p = await createAdapter().fileSave({
      filters: [{name: kind.label, extensions: [kind.ext]}],
    })
    if (!p) return ''
    path = p
  }

  const res: {data: string} = await request(kind.endpoint, {
    body: {flowId, path},
    method: 'POST',
    timeoutMs: EXPORT_TIMEOUT_MS,
  })

  if (!getPlatformCapabilities().nativeDialogs && res.data) {
    const link = document.createElement('a')
    link.href = `data:${kind.mime};base64,${res.data}`
    link.download = `export.${kind.ext}`
    document.body.appendChild(link)
    link.click()
    document.body.removeChild(link)
  }
  return path
}

export const exportApi = {
  exportPDF: (flowId?: string): Promise<string> =>
    exportReport({endpoint: '/api/export/pdf', mime: 'application/pdf', label: 'PDF', ext: 'pdf'}, flowId),

  exportMarkdown: (flowId?: string): Promise<string> =>
    exportReport({endpoint: '/api/export/markdown', mime: 'text/markdown', label: 'Markdown', ext: 'md'}, flowId),

  exportHTML: (flowId?: string): Promise<string> =>
    exportReport({endpoint: '/api/export/html', mime: 'text/html', label: 'HTML', ext: 'html'}, flowId),

  saveTextExport: async (content: string, kind: {label: string; ext: string}): Promise<string> => {
    const path = await createAdapter().fileSave({filters: [{name: kind.label, extensions: [kind.ext]}]})
    if (!path) return ''
    await request('/api/export/save', {body: {path, content}, method: 'POST', timeoutMs: EXPORT_TIMEOUT_MS})
    return path
  },

  compareCurrentWith: (path: string): Promise<FlowDiff> => request('/api/export/compare', {body: {path}}),

  pickFile: async (filter: string): Promise<string | null> => {
    const extensions = filter ? filter.split(',') : undefined
    const path = await createAdapter().fileOpen({
      filters: extensions ? [{name: 'Files', extensions}] : undefined,
    })
    return Array.isArray(path) ? (path[0] ?? null) : path
  },
}
