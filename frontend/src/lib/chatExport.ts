import type {ChatMessage} from '@/types'
import {modelNameOf} from './modelIds'
import {triggerDownload} from './download'

export type ExportModelName = (message: ChatMessage) => string | undefined

const defaultModelName: ExportModelName = m => modelNameOf(m.model, m.modelLabel) ?? undefined

export function conversationToMarkdown(
  title: string,
  messages: ChatMessage[],
  modelName: ExportModelName = defaultModelName,
): string {
  const lines: string[] = [`# ${title || 'Chat conversation'}`, '']
  for (const m of messages) {
    if (m.finishReason === 'error') continue
    const name = m.role === 'assistant' ? modelName(m) : undefined
    const who = m.role === 'user' ? 'You' : m.role === 'assistant' ? (name ? `AI (${name})` : 'AI') : m.role
    const when = formatWhen(m.timestamp)
    lines.push(`## ${who}${when ? ` · ${when}` : ''}`, '', m.content.trim(), '')
  }
  return lines.join('\n')
}

function formatWhen(ts: string): string {
  const d = new Date(ts)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString()
}

export function downloadTextFile(filename: string, content: string, mime = 'text/markdown'): void {
  triggerDownload(new Blob([content], {type: mime}), filename)
}

export function safeFilename(title: string): string {
  const base = (title || 'conversation')
    .normalize('NFC')
    .replace(/[^\p{L}\p{M}\p{N}]+/gu, '-')
    .replace(/^-+|-+$/g, '')
    .toLowerCase()
  return `chat-${base || 'conversation'}.md`
}
