import {libraryApi} from '@/api/library'
import {useFlowStore} from '@/stores/flowStore'
import type {FlowDocument} from '@/types'

/**
 * Reloads the open library flow from the server: its version and its document, swapped in place as a remote update
 * (useFlowChangeSync does the same for a flow.changed from the presence socket). Nothing changes when another flow
 * was opened meanwhile.
 */
export async function reloadLibraryDocument(flowId: string, signal?: AbortSignal): Promise<void> {
  const [meta, content] = await Promise.all([libraryApi.get(flowId, signal), libraryApi.getContent(flowId, signal)])
  if (useFlowStore.getState().document?.id !== flowId) return
  if (typeof meta.version === 'number') useFlowStore.setState({libraryVersion: meta.version})
  useFlowStore.getState().applyRemoteDocumentUpdate(content as FlowDocument)
}
