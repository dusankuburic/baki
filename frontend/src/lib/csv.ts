// Shared CSV and download utilities used by all CSV-export features.

// csvCell wraps a value in double quotes, escapes embedded quotes, and
// neutralises formula-injection attacks (= + - @) by prefixing a single quote.
export function csvCell(value: string): string {
  if (/^[=+\-@]/.test(value)) value = `'${value}`
  const escaped = value.replace(/"/g, '""')
  const needsQuote = /[",\n\r]/.test(escaped)
  return needsQuote ? `"${escaped}"` : escaped
}

export function downloadBlob(content: string, mime: string, filename: string): void {
  const blob = new Blob([content], {type: mime})
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}
