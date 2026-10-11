import {triggerDownload} from './download'

export function csvCell(value: string): string {
  if (/^[=+\-@]/.test(value)) value = `'${value}`
  const escaped = value.replace(/"/g, '""')
  const needsQuote = /[",\n\r]/.test(escaped)
  return needsQuote ? `"${escaped}"` : escaped
}

export function downloadBlob(content: string, mime: string, filename: string): void {
  triggerDownload(new Blob([content], {type: mime}), filename)
}
