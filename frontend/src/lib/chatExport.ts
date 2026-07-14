import type {ChatMessage} from '@/types'

// conversationToMarkdown renders a chat thread as a portable Markdown
// transcript. Works in every deployment (no backend/native dialog needed),
// so cloud users can export history too — the server-side export endpoint is
// desktop-only. Error turns are dropped so the transcript isn't littered with
// transient failures.
export function conversationToMarkdown(title: string, messages: ChatMessage[]): string {
  const lines: string[] = [`# ${title || 'Chat conversation'}`, '']
  for (const m of messages) {
    if (m.finishReason === 'error') continue
    const who = m.role === 'user' ? 'You' : m.role === 'assistant' ? (m.model ? `AI (${m.model})` : 'AI') : m.role
    const when = formatWhen(m.timestamp)
    lines.push(`## ${who}${when ? ` · ${when}` : ''}`, '', m.content.trim(), '')
  }
  return lines.join('\n')
}

function formatWhen(ts: string): string {
  const d = new Date(ts)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString()
}

// downloadTextFile triggers a browser download of text content. Same pattern as
// the account-data export — works in web and the Tauri webview alike.
export function downloadTextFile(filename: string, content: string, mime = 'text/markdown'): void {
  const blob = new Blob([content], {type: mime})
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}

// safeFilename sanitizes a thread title into a filesystem-safe basename.
export function safeFilename(title: string): string {
  const base = (title || 'conversation')
    .replace(/[^a-z0-9]+/gi, '-')
    .replace(/^-+|-+$/g, '')
    .toLowerCase()
  return `chat-${base || 'conversation'}.md`
}
