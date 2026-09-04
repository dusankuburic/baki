import {useTranslation} from 'react-i18next'
// MentionPill is a module-level markdown component and deliberately hook-free,
// so its copy goes through the i18next instance rather than the hook.
import i18n from '@/i18n'
import clsx from 'clsx'
import {Copy, Check, RefreshCw, RotateCcw, Bot, User, CircleSlash} from 'lucide-react'
import {useCallback, memo} from 'react'
import ReactMarkdown, {defaultUrlTransform} from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type {ChatMessage as ChatMessageType} from '@/types'
import {useCopy} from '@/hooks/useCopy'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import CodeBlock from './CodeBlock'
import {ToolTrail, FixOutcomeStrip} from './ToolTrail'

interface Props {
  message: ChatMessageType
  isStreaming?: boolean
  isThinking?: boolean
  isLastAssistant?: boolean
  onRegenerate?: () => void
  onRetry?: () => void
  // Active search query — occurrences are <mark>ed in the rendered text.
  highlight?: string
  // True for the match the search bar is currently stepping through.
  isActiveMatch?: boolean
}

function formatTime(ts: string): string {
  try {
    const d = new Date(ts)
    return d.toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'})
  } catch {
    return ''
  }
}

const MentionPill = ({path}: {path: string}) => {
  const parts = path.split(/[/\\]/)
  const filename = parts[parts.length - 1]
  // Clicking a mention jumps the graph to the matching subflow (imperative
  // store access so this module-level markdown component stays hook-free).
  const handleClick = () => {
    const ok = useFlowStore.getState().navigateToSourceFile(path)
    if (ok) useUIStore.getState().setMainPaneView('graph')
  }
  return (
    <button
      type="button"
      onClick={handleClick}
      title={i18n.t('chat:message.goTo', {name: filename})}
      aria-label={i18n.t('chat:message.goTo', {name: filename})}
      className="inline-flex items-center px-1.5 py-0.5 rounded-md bg-brand-500/10 text-brand-400 border border-brand-500/20 hover:bg-brand-500/20 font-medium text-[0.9em] mx-0.5 transition-colors"
    >
      @{filename}
    </button>
  )
}

// BlockLink intercepts markdown links using the `block:<id>` scheme the model
// is prompted to emit, turning a reference into a jump to that block in the
// graph instead of a page navigation.
const BlockLink = ({href, children}: {href?: string; children?: React.ReactNode}) => {
  if (href && href.startsWith('block:')) {
    const blockId = href.slice('block:'.length)
    const handleClick = (e: React.MouseEvent) => {
      e.preventDefault()
      useFlowStore.getState().navigateToBlock(blockId)
      useUIStore.getState().setMainPaneView('graph')
      useUIStore.getState().setInspectorTab('details')
    }
    return (
      <button
        type="button"
        onClick={handleClick}
        className="text-brand-400 hover:text-brand-300 underline underline-offset-2"
      >
        {children}
      </button>
    )
  }
  if (href && href.startsWith('finding:')) {
    const key = href.slice('finding:'.length)
    const handleClick = (e: React.MouseEvent) => {
      e.preventDefault()
      useUIStore.getState().setInspectorTab('findings')
      useAnalysisStore.getState().setFocusedFinding(key)
    }
    return (
      <button
        type="button"
        onClick={handleClick}
        className="text-brand-400 hover:text-brand-300 underline underline-offset-2"
      >
        {children}
      </button>
    )
  }
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className="text-brand-400 hover:text-brand-300 underline underline-offset-2"
    >
      {children}
    </a>
  )
}

interface MarkdownPreProps {
  children?: React.ReactNode
}

interface MarkdownCodeProps {
  node?: unknown
  inline?: boolean
  className?: string
  children?: React.ReactNode
}

// The @-mention shape, shared by the highlighter below. Kept as a source
// fragment (not a RegExp) because it is spliced into a larger alternation.
const MENTION_PATTERN = String.raw`@[a-zA-Z0-9_./\\-]+`

const markdownPlugins = [remarkGfm]

// escapeRegExp: the search query is user input and goes straight into a RegExp.
function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

// segmentPattern builds the alternation used to split a text run into
// @-mentions and (when searching) query hits. Returns null when there is
// nothing to look for.
function segmentPattern(text: string, highlight?: string): {re: RegExp; q?: string} | null {
  const hasMention = text.includes('@')
  const q = highlight?.trim()
  if (!hasMention && !q) return null
  const alt = [q ? `(${escapeRegExp(q)})` : null, hasMention ? `(${MENTION_PATTERN})` : null].filter(Boolean).join('|')
  return {re: new RegExp(alt, 'gi'), q}
}

// --- hast plumbing -------------------------------------------------------
//
// react-markdown's `components` map is keyed by ELEMENT name, so a `text` entry
// is silently ignored — the previous `components.text` override never ran, and
// @-mentions in assistant answers rendered as plain text for as long as it has
// been there (user messages looked right only because they bypass markdown).
//
// Splitting text therefore has to happen in the tree, before rendering: this
// rehype plugin rewrites text nodes into custom elements that `components`
// below can then map to real React components.

interface HastNode {
  type: string
  tagName?: string
  value?: string
  properties?: Record<string, unknown>
  children?: HastNode[]
}

const MENTION_TAG = 'chat-mention'
const MARK_TAG = 'chat-mark'
// Code keeps its literal text: a pill or a highlight inside a snippet would
// misrepresent what the model actually wrote.
const OPAQUE_TAGS = new Set(['code', 'pre'])

function splitTextNode(value: string, highlight?: string): HastNode[] | null {
  const found = segmentPattern(value, highlight)
  if (!found) return null
  const parts = value.split(found.re).filter(p => p !== undefined && p !== '')

  let changed = false
  const nodes = parts.map<HastNode>(part => {
    if (found.q && part.toLowerCase() === found.q.toLowerCase()) {
      changed = true
      return {type: 'element', tagName: MARK_TAG, properties: {}, children: [{type: 'text', value: part}]}
    }
    if (part.startsWith('@') && part.length > 1) {
      changed = true
      return {
        type: 'element',
        tagName: MENTION_TAG,
        properties: {path: part.slice(1)},
        children: [{type: 'text', value: part}],
      }
    }
    return {type: 'text', value: part}
  })

  // Bail only when nothing was actually wrapped. Counting parts instead missed
  // the case where the WHOLE text node is the match (e.g. the sole child of a
  // <strong>), which split() returns as a single part.
  return changed ? nodes : null
}

function rehypeChatText(highlight?: string) {
  return () => (tree: HastNode) => {
    const walk = (node: HastNode) => {
      if (!node.children) return
      if (node.tagName && OPAQUE_TAGS.has(node.tagName)) return
      const next: HastNode[] = []
      for (const child of node.children) {
        if (child.type === 'text' && typeof child.value === 'string') {
          const replacement = splitTextNode(child.value, highlight)
          if (replacement) {
            next.push(...replacement)
            continue
          }
        }
        walk(child)
        next.push(child)
      }
      node.children = next
    }
    walk(tree)
  }
}

// renderTextSegment is the same transformation for PLAIN text (user messages,
// which never go through markdown), so both sides of the conversation
// highlight and pill identically.
function renderTextSegment(text: string, highlight?: string): React.ReactNode {
  const found = segmentPattern(text, highlight)
  if (!found) return text
  const parts = text.split(found.re).filter(p => p !== undefined && p !== '')

  return (
    <>
      {parts.map((part, i) => {
        if (found.q && part.toLowerCase() === found.q.toLowerCase()) {
          return (
            <mark key={i} className="chat-search-hit">
              {part}
            </mark>
          )
        }
        if (part.startsWith('@') && part.length > 1) {
          return <MentionPill key={i} path={part.slice(1)} />
        }
        return <span key={i}>{part}</span>
      })}
    </>
  )
}

// react-markdown strips URLs with unknown schemes by default; allow our
// internal "block:" / "finding:" deep-link schemes through so BlockLink can
// intercept them. Everything else still goes through the default (XSS-safe)
// transform.
const urlTransform = (url: string) =>
  url.startsWith('block:') || url.startsWith('finding:') ? url : defaultUrlTransform(url)

// The plugin list must keep a stable identity per query — react-markdown
// re-parses everything when it changes. Searching is a discrete mode (never
// concurrent with streaming), so a one-entry cache keeps re-renders off the
// streaming path. The components map is query-independent.
let pluginCacheKey: string | undefined
let pluginCacheValue: [ReturnType<typeof rehypeChatText>] | undefined

function getRehypePlugins(highlight?: string) {
  const key = highlight ?? ''
  if (pluginCacheKey === key && pluginCacheValue) return pluginCacheValue
  pluginCacheKey = key
  pluginCacheValue = [rehypeChatText(highlight)]
  return pluginCacheValue
}

const markdownComponents = {
  pre({children}: MarkdownPreProps) {
    let codeProps: Record<string, unknown> = {}
    if (children && typeof children === 'object' && 'props' in (children as object)) {
      codeProps = (children as {props: Record<string, unknown>}).props
    } else if (Array.isArray(children) && children[0] && typeof children[0] === 'object' && 'props' in children[0]) {
      codeProps = (children[0] as {props: Record<string, unknown>}).props
    }

    const className = (codeProps.className as string) || ''
    const match = /language-(\w+)/.exec(className)
    const language = match ? match[1] : ''
    const value = String(codeProps.children || '').replace(/\n$/, '')

    return <CodeBlock language={language} value={value} />
  },
  // Inline code. Its look is owned by `.prose-chat code` in index.css —
  // repeating it in utilities here was misleading, because `.prose-chat code`
  // (0-1-1) outranks a single-class utility (0-1-0), so the background,
  // padding and size declared here never actually applied.
  code({inline: _inline, className, children, ...props}: MarkdownCodeProps) {
    return (
      <code className={className} {...props}>
        {children}
      </code>
    )
  },
  a({href, children}: {href?: string; children?: React.ReactNode}) {
    return <BlockLink href={href}>{children}</BlockLink>
  },
  // The elements rehypeChatText produced. `components` is keyed by element
  // name, which is why these work where a `text` key could not.
  [MENTION_TAG]({path, children}: {path?: string; children?: React.ReactNode}) {
    return path ? <MentionPill path={path} /> : <>{children}</>
  },
  [MARK_TAG]({children}: {children?: React.ReactNode}) {
    return <mark className="chat-search-hit">{children}</mark>
  },
}

// StableMarkdown re-renders only when its content string changes (memo
// compares strings by value). During streaming the completed portion of the
// message routes through it, so react-markdown re-parses only the growing
// tail on each animation-frame flush instead of the whole message.
const StableMarkdown = memo(function StableMarkdown({content, highlight}: {content: string; highlight?: string}) {
  return (
    <ReactMarkdown
      remarkPlugins={markdownPlugins}
      rehypePlugins={getRehypePlugins(highlight)}
      components={markdownComponents}
      urlTransform={urlTransform}
    >
      {content}
    </ReactMarkdown>
  )
})

// splitStreamingContent splits streamed markdown at the last completed
// paragraph boundary so the (large) head can be memoized. It never splits
// inside an unterminated ``` fence — a fence torn across two parsers would
// render as garbage. Constructs that span blank lines (loose lists, multi-
// paragraph quotes) may render with slightly different spacing while the
// stream is live; the final message is parsed in one piece as before.
export function splitStreamingContent(content: string): [head: string, tail: string] {
  const idx = content.lastIndexOf('\n\n')
  if (idx < 0) return ['', content]
  const head = content.slice(0, idx + 2)
  const fences = (head.match(/```/g) || []).length
  if (fences % 2 === 1) return ['', content]
  return [head, content.slice(idx + 2)]
}

// splitOpenFence locates an UNTERMINATED ``` fence and returns the prose
// before it plus the code accumulated so far. While a fence is open,
// splitStreamingContent deliberately refuses to advance the memoized head, so
// without this the entire message was re-parsed by react-markdown AND
// re-tokenized by Prism on every animation frame. `before` cannot change until
// the fence closes, so it memoizes perfectly and the only per-frame work left
// is appending text to a plain <pre>.
export function splitOpenFence(content: string): {before: string; lang: string; code: string} | null {
  const fences = content.match(/```/g)
  if (!fences || fences.length % 2 === 0) return null
  const idx = content.lastIndexOf('```')
  const rest = content.slice(idx + 3)
  const nl = rest.indexOf('\n')
  return {
    before: content.slice(0, idx),
    lang: (nl === -1 ? rest : rest.slice(0, nl)).trim(),
    code: nl === -1 ? '' : rest.slice(nl + 1),
  }
}

function renderContent(content: string, isUser: boolean, isStreaming?: boolean, highlight?: string) {
  if (isUser) {
    return <div className="whitespace-pre-wrap break-words">{renderTextSegment(content, highlight)}</div>
  }

  if (isStreaming) {
    const open = splitOpenFence(content)
    if (open) {
      return (
        <div className="prose-chat break-words is-streaming">
          {open.before !== '' && <StableMarkdown content={open.before} highlight={highlight} />}
          <CodeBlock language={open.lang} value={open.code} plain />
          <span className="streaming-cursor inline-block w-[3px] h-[1.2em] bg-brand-400 ml-0.5 align-text-bottom" />
        </div>
      )
    }
    const [head, tail] = splitStreamingContent(content)
    return (
      <div className="prose-chat break-words is-streaming">
        {head !== '' && <StableMarkdown content={head} highlight={highlight} />}
        <ReactMarkdown
          remarkPlugins={markdownPlugins}
          rehypePlugins={getRehypePlugins(highlight)}
          components={markdownComponents}
          urlTransform={urlTransform}
        >
          {tail}
        </ReactMarkdown>
        <span className="streaming-cursor inline-block w-[3px] h-[1.2em] bg-brand-400 ml-0.5 align-text-bottom" />
      </div>
    )
  }

  return (
    <div className="prose-chat break-words">
      <ReactMarkdown
        remarkPlugins={markdownPlugins}
        rehypePlugins={getRehypePlugins(highlight)}
        components={markdownComponents}
        urlTransform={urlTransform}
      >
        {content}
      </ReactMarkdown>
    </div>
  )
}

function MessageBubble({
  message,
  isStreaming,
  isThinking,
  isLastAssistant,
  onRegenerate,
  onRetry,
  highlight,
  isActiveMatch,
}: Props) {
  const {t} = useTranslation('chat')
  const isUser = message.role === 'user'
  const isError = message.finishReason === 'error'
  const isInterrupted = message.finishReason === 'interrupted'
  const {copied, copy} = useCopy()

  const handleCopy = useCallback(() => {
    copy(message.content)
  }, [message.content, copy])

  const time = formatTime(message.timestamp)

  if (isThinking) {
    return (
      <div className="flex flex-col items-start gap-1" role="status">
        <div className="flex items-center gap-1.5 px-1">
          <Bot size={11} className="text-text-tertiary" />
          <span className="text-2xs font-medium text-text-tertiary">AI</span>
        </div>
        <div className="px-4 py-3 bg-surface-2 border border-border-subtle rounded-2xl rounded-tl-md">
          <div className="flex items-center gap-2">
            <div className="flex gap-1" aria-hidden="true">
              <span className="typing-dot w-1.5 h-1.5 rounded-full bg-text-tertiary" style={{animationDelay: '0ms'}} />
              <span
                className="typing-dot w-1.5 h-1.5 rounded-full bg-text-tertiary"
                style={{animationDelay: '150ms'}}
              />
              <span
                className="typing-dot w-1.5 h-1.5 rounded-full bg-text-tertiary"
                style={{animationDelay: '300ms'}}
              />
            </div>
            <span className="text-xs text-text-tertiary">{t('message.thinking')}</span>
          </div>
        </div>
      </div>
    )
  }

  return (
    // No mount animation: inside a virtualized list, items mount whenever they
    // scroll back into view, so animate-message-in made history flicker.
    <div
      className={clsx(
        'group flex flex-col gap-1 rounded-lg transition-shadow',
        isUser ? 'items-end' : 'items-start',
        isActiveMatch && 'ring-1 ring-brand-500/50 ring-offset-2 ring-offset-surface-1',
      )}
    >
      <div className="flex items-center gap-1.5 px-1">
        {isUser ? <User size={11} className="text-text-tertiary" /> : <Bot size={11} className="text-text-tertiary" />}
        <span className="text-2xs font-medium text-text-tertiary">
          {isUser ? t('message.roleUser') : t('message.roleAssistant')}
        </span>
        {message.model && !isUser && <span className="text-2xs text-text-tertiary/60">· {message.model}</span>}
        {time && <span className="text-2xs text-text-tertiary/40">{time}</span>}
      </div>

      {/* The user's turn keeps a bubble (it is short and benefits from the
          right-aligned shape). The assistant runs full-bleed: in a 280-560px
          panel, a border + 12px of horizontal padding was a measurable bite
          out of an already narrow measure, for no information. Errors keep a
          left accent so they still read as set apart. */}
      <div
        className={clsx(
          'relative max-w-full text-sm leading-relaxed',
          isUser
            ? 'px-3 py-2.5 max-w-[85%] bg-brand-500/12 border border-brand-500/20 rounded-2xl rounded-tr-sm'
            : isError
              ? 'w-full py-1 pl-3 border-l-2 border-semantic-error/50 text-text-secondary'
              : 'w-full',
        )}
      >
        {renderContent(message.content, isUser, isStreaming, highlight)}
      </div>

      {!isUser &&
        (message.fixProposals ?? (message.fixProposal ? [message.fixProposal] : [])).map(snap => (
          // A still-pending card on an interrupted message is DEAD: stopping
          // the stream cancels the apply_fix tool call server-side, so the
          // decision can never resolve — say so instead of showing an
          // ambiguous pending strip (U4.1).
          <FixOutcomeStrip
            key={snap.proposalId}
            snapshot={isInterrupted && snap.status === 'pending' ? {...snap, status: 'cancelled'} : snap}
          />
        ))}

      {!isUser && !isStreaming && message.toolCalls && message.toolCalls.length > 0 && (
        <ToolTrail calls={message.toolCalls} />
      )}

      {isInterrupted && (
        <div className="flex items-center gap-1 px-1 text-2xs text-text-tertiary">
          <CircleSlash size={10} />
          <span>{t('message.stopped')}</span>
        </div>
      )}

      {!isStreaming && (
        <div
          // U1.6: actions reveal on hover AND on keyboard focus-within —
          // hover-only opacity left keyboard users with invisible-but-
          // focusable buttons (the FindingsList pattern). Pure CSS off the
          // container's `group`: the old useState re-rendered a markdown-heavy
          // component on every pointer enter/leave.
          className="flex items-center gap-1 px-1 opacity-0 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100"
        >
          <button
            className="p-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-text-secondary transition-colors"
            onClick={handleCopy}
            aria-label={copied ? t('message.copied') : t('message.copy')}
          >
            {copied ? <Check size={11} className="text-green-400" /> : <Copy size={11} />}
          </button>
          {isError && onRetry && (
            <button
              className="flex items-center gap-1 p-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-red-400 transition-colors text-2xs"
              onClick={onRetry}
            >
              <RotateCcw size={11} />
              <span>{t('message.retry')}</span>
            </button>
          )}
          {isLastAssistant && !isError && onRegenerate && (
            <button
              className="flex items-center gap-1 p-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-text-secondary transition-colors text-2xs"
              onClick={onRegenerate}
            >
              <RefreshCw size={11} />
              <span>{t('message.regenerate')}</span>
            </button>
          )}
        </div>
      )}
    </div>
  )
}

// Memoize to prevent re-renders during streaming - only re-render when props change
export default memo(MessageBubble, (prevProps, nextProps) => {
  return (
    prevProps.message.id === nextProps.message.id &&
    prevProps.message.content === nextProps.message.content &&
    prevProps.message.finishReason === nextProps.message.finishReason &&
    prevProps.message.toolCalls === nextProps.message.toolCalls &&
    prevProps.message.fixProposal === nextProps.message.fixProposal &&
    prevProps.message.fixProposals === nextProps.message.fixProposals &&
    prevProps.isStreaming === nextProps.isStreaming &&
    prevProps.isThinking === nextProps.isThinking &&
    prevProps.isLastAssistant === nextProps.isLastAssistant &&
    prevProps.onRegenerate === nextProps.onRegenerate &&
    prevProps.onRetry === nextProps.onRetry &&
    prevProps.highlight === nextProps.highlight &&
    prevProps.isActiveMatch === nextProps.isActiveMatch
  )
})
