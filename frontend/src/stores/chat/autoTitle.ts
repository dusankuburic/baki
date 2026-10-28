/** The longest title a thread gets from its first user message. */
export const AUTO_TITLE_MAX = 50

/**
 * The thread title derived from its first user message (R-frontend-19): runs of whitespace (newlines included)
 * collapse to one space, the ends are trimmed, and the result is cut to AUTO_TITLE_MAX characters. Whitespace-only
 * text gives '' (no title).
 */
export function autoTitle(text: string): string {
  return text.replace(/\s+/g, ' ').trim().slice(0, AUTO_TITLE_MAX).trimEnd()
}
