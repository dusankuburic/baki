import {useTranslation} from 'react-i18next'
import i18n from '@/i18n'
import clsx from 'clsx'
import {
  Copy,
  Check,
  RefreshCw,
  RotateCcw,
  Bot,
  User,
  CircleSlash,
  Settings,
  Users,
  ListFilter,
  ExternalLink as ExternalLinkIcon,
  Scissors,
  BookX,
} from 'lucide-react'
import {useCallback, useId, memo, useState} from 'react'
import ReactMarkdown, {defaultUrlTransform} from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type {ChatMessage as ChatMessageType} from '@/types'
import type {KnowledgeStatus} from '@/lib/chatEvent'
import {useCopy} from '@/hooks/useCopy'
import {chatErrorGuidance, providerErrorInfo} from '@/lib/chatErrors'
import {formatTimeOfDay} from '@/lib/format'
import {isRegistryModelId, storableModelLabel} from '@/lib/modelIds'
import {useChatStore} from '@/stores/chatStore'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import CodeBlock from './CodeBlock'
import ThinkingIndicator from './ThinkingIndicator'
import {ToolTrail, FixOutcomeStrip, ApprovalOutcomeStrip} from './ToolTrail'
import {settledCard} from './settledCard'

interface Props {
  message: ChatMessageType
  modelLabel?: string
  isStreaming?: boolean
  isThinking?: boolean
  isLastAssistant?: boolean
  onRegenerate?: () => void
  onRetry?: () => void
  resendDisabledReason?: string
  highlight?: string
  isActiveMatch?: boolean
  /** Compacts the thread; offered on the omitted-history chip of a turn that lost earlier turns. */
  onCompact?: () => void
}

const MentionPill = ({path}: {path: string}) => {
  const parts = path.split(/[/\\]/)
  const filename = parts[parts.length - 1]
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
      className="inline-flex items-center max-w-full px-1.5 py-0.5 rounded-md bg-brand-500/10 text-brand-400 border border-brand-500/20 hover:bg-brand-500/20 font-medium text-[0.9em] mx-0.5 transition-colors"
    >
      <span className="truncate">@{filename}</span>
    </button>
  )
}

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
  return <ExternalLink href={href}>{children}</ExternalLink>
}

// A URL the model emits is as trustworthy as the flow and the documents it read (the system prompt tells it to
// write URLs it finds as plain text), so it is never a navigable link until the user asks to see where it goes:
// the first click shows the address as the link.
const ExternalLink = ({href, children}: {href?: string; children?: React.ReactNode}) => {
  const [revealed, setRevealed] = useState(false)
  if (!href) return <>{children}</>
  if (revealed) {
    return (
      <a
        href={href}
        target="_blank"
        rel="noopener noreferrer"
        className="text-brand-400 hover:text-brand-300 underline underline-offset-2 break-all"
      >
        {children} ({href})
      </a>
    )
  }
  return (
    <button
      type="button"
      onClick={() => setRevealed(true)}
      title={i18n.t('chat:message.externalLink', {url: href})}
      className="inline-flex items-center gap-0.5 text-brand-400 hover:text-brand-300 underline decoration-dotted underline-offset-2"
    >
      {children}
      <ExternalLinkIcon size={10} aria-hidden="true" />
    </button>
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

// The knowledge notice's copy per status (R-knowledge-1).
const KNOWLEDGE_NOTICE = {
  identity_mismatch: 'message.knowledge.identityMismatch',
  no_embedder: 'message.knowledge.noEmbedder',
  deadline: 'message.knowledge.deadline',
} as const satisfies Record<KnowledgeStatus, string>

const MENTION_PATTERN = String.raw`@[a-zA-Z0-9_./\\-]+`

const markdownPlugins = [remarkGfm]

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

function segmentPattern(text: string, highlight?: string): {re: RegExp; q?: string} | null {
  const hasMention = text.includes('@')
  const q = highlight?.trim()
  if (!hasMention && !q) return null
  const alt = [q ? `(${escapeRegExp(q)})` : null, hasMention ? `(${MENTION_PATTERN})` : null].filter(Boolean).join('|')
  return {re: new RegExp(alt, 'gi'), q}
}

interface HastNode {
  type: string
  tagName?: string
  value?: string
  properties?: Record<string, unknown>
  children?: HastNode[]
}

const MENTION_TAG = 'chat-mention'
const MARK_TAG = 'chat-mark'
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

const urlTransform = (url: string) =>
  url.startsWith('block:') || url.startsWith('finding:') ? url : defaultUrlTransform(url)

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
  table({node: _node, children, ...props}: React.TableHTMLAttributes<HTMLTableElement> & {node?: unknown}) {
    return (
      <div className="chat-table-wrap">
        <table {...props}>{children}</table>
      </div>
    )
  },
  [MENTION_TAG]({path, children}: {path?: string; children?: React.ReactNode}) {
    return path ? <MentionPill path={path} /> : <>{children}</>
  },
  [MARK_TAG]({children}: {children?: React.ReactNode}) {
    return <mark className="chat-search-hit">{children}</mark>
  },
}

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

export function splitStreamingContent(content: string): [head: string, tail: string] {
  const idx = content.lastIndexOf('\n\n')
  if (idx < 0) return ['', content]
  const head = content.slice(0, idx + 2)
  const fences = (head.match(/```/g) || []).length
  if (fences % 2 === 1) return ['', content]
  return [head, content.slice(idx + 2)]
}

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
    return <div className="whitespace-pre-wrap wrap-anywhere">{renderTextSegment(content, highlight)}</div>
  }

  if (isStreaming) {
    const open = splitOpenFence(content)
    if (open) {
      return (
        <div className="prose-chat is-streaming">
          {open.before !== '' && <StableMarkdown content={open.before} highlight={highlight} />}
          <CodeBlock language={open.lang} value={open.code} plain />
          <span className="streaming-cursor inline-block w-[3px] h-[1.2em] bg-brand-400 ml-0.5 align-text-bottom" />
        </div>
      )
    }
    const [head, tail] = splitStreamingContent(content)
    return (
      <div className="prose-chat is-streaming">
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
    <div className="prose-chat">
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
  modelLabel,
  isStreaming,
  isThinking,
  isLastAssistant,
  onRegenerate,
  onRetry,
  resendDisabledReason,
  highlight,
  isActiveMatch,
  onCompact,
}: Props) {
  const {t} = useTranslation('chat')
  const isUser = message.role === 'user'
  const isError = message.finishReason === 'error'
  const isInterrupted = message.finishReason === 'interrupted'
  const providerError = isError ? providerErrorInfo(message.errorCode) : null
  const guidance = isError ? chatErrorGuidance(message.errorCode) : null
  const offersRetry = isError && !!onRetry && !guidance?.hideRetry
  const openSettings = useUIStore(s => s.openSettings)
  const {copied, copy} = useCopy()
  const resendReasonId = useId()

  const handleCopy = useCallback(() => {
    copy(message.content)
  }, [message.content, copy])

  const time = formatTimeOfDay(message.timestamp)
  const modelName = message.model
    ? storableModelLabel(message.modelLabel, message.model) ||
      modelLabel ||
      (isRegistryModelId(message.model) ? t('message.removedModel') : message.model)
    : ''
  const resendProps = resendDisabledReason
    ? {'aria-disabled': true, 'aria-describedby': resendReasonId, title: resendDisabledReason}
    : {}
  const showsResend = offersRetry || (isLastAssistant && !isError && !!onRegenerate)

  if (isThinking) return <ThinkingIndicator />

  return (
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
        {modelName && !isUser && <span className="text-2xs text-text-tertiary/60">· {modelName}</span>}
        {message.servedModel &&
          !isUser &&
          message.servedModel !== message.model &&
          message.servedModel !== modelName && (
            <span
              className="text-2xs text-text-tertiary/60"
              role="img"
              aria-label={t('message.servedBy', {model: message.servedModel})}
              title={t('message.servedBy', {model: message.servedModel})}
            >
              · {message.servedModel}
            </span>
          )}
        {time && <span className="text-2xs text-text-tertiary/40">{time}</span>}
      </div>

      <div
        className={clsx(
          'relative max-w-full chat-text leading-relaxed',
          isUser
            ? 'px-3 py-2.5 max-w-[85%] bg-brand-500/10 border border-brand-500/20 rounded-2xl rounded-tr-sm'
            : isError
              ? 'w-full py-1 pl-3 border-l-2 border-semantic-error/50 text-text-secondary'
              : 'w-full',
        )}
      >
        {renderContent(message.content, isUser, isStreaming, highlight)}
      </div>

      {isError && providerError?.action === 'chooseModel' && (
        <button
          type="button"
          className="flex items-center gap-1 px-1 text-2xs text-brand-400 hover:text-brand-300 transition-colors"
          onClick={() => useChatStore.getState().openModelPicker()}
        >
          <ListFilter size={11} aria-hidden="true" />
          <span>{t('errors.chooseModel')}</span>
        </button>
      )}

      {isError &&
        providerError &&
        !providerError.action &&
        !(message.errorShared && message.errorCode === 'PROVIDER_NOT_CONFIGURED') && (
          <button
            type="button"
            className="flex items-center gap-1 px-1 text-2xs text-brand-400 hover:text-brand-300 transition-colors"
            onClick={() => openSettings(providerError.settingsSection)}
          >
            <Settings size={11} />
            <span>{t('errors.openSettings')}</span>
          </button>
        )}

      {guidance?.action === 'compact' && onCompact && (
        <button
          type="button"
          className="flex items-center gap-1 px-1 text-2xs text-brand-400 hover:text-brand-300 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
          onClick={onCompact}
          disabled={!!resendDisabledReason}
          title={resendDisabledReason}
        >
          <Scissors size={11} aria-hidden="true" />
          <span>{t('message.compact')}</span>
        </button>
      )}

      {guidance?.hintKey && <p className="px-1 text-2xs text-text-tertiary">{t(guidance.hintKey)}</p>}

      {isError && message.errorShared && (
        <button
          type="button"
          className="flex items-center gap-1 px-1 text-2xs text-brand-400 hover:text-brand-300 transition-colors"
          onClick={() => openSettings('orgs')}
        >
          <Users size={11} />
          <span>{t('errors.openOrgSettings')}</span>
        </button>
      )}

      {!isUser &&
        (message.fixProposals ?? (message.fixProposal ? [message.fixProposal] : [])).map(snap => (
          <FixOutcomeStrip key={snap.proposalId} snapshot={settledCard(snap, isInterrupted)} />
        ))}

      {!isUser &&
        (message.approvals ?? []).map(snap => (
          <ApprovalOutcomeStrip key={snap.proposalId} snapshot={settledCard(snap, isInterrupted)} />
        ))}

      {!isUser && !isStreaming && message.toolCalls && message.toolCalls.length > 0 && (
        <ToolTrail calls={message.toolCalls} />
      )}

      {!isUser && !!message.truncated && (
        <div className="flex items-center gap-1.5 px-1 text-2xs text-text-tertiary" data-testid="omitted-history">
          <Scissors size={10} aria-hidden="true" />
          <span>{t('message.omitted', {count: message.truncated})}</span>
          {onCompact && (
            <button
              type="button"
              onClick={onCompact}
              className="text-brand-400 hover:text-brand-300 underline underline-offset-2"
            >
              {t('message.compact')}
            </button>
          )}
        </div>
      )}

      {!isUser && message.knowledge && (
        <div className="flex items-start gap-1.5 px-1 text-2xs text-text-tertiary" data-testid="knowledge-notice">
          <BookX size={10} aria-hidden="true" className="mt-0.5 shrink-0" />
          <span>{t(KNOWLEDGE_NOTICE[message.knowledge])}</span>
        </div>
      )}

      {isInterrupted && (
        <div className="flex items-center gap-1 px-1 text-2xs text-text-tertiary">
          <CircleSlash size={10} />
          <span>{t('message.stopped')}</span>
        </div>
      )}

      {!isStreaming && (
        <div
          className="flex items-center gap-1 px-1 opacity-0 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100"
        >
          <button
            className="p-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-text-secondary transition-colors"
            onClick={handleCopy}
            aria-label={copied ? t('message.copied') : t('message.copy')}
          >
            {copied ? <Check size={11} className="text-green-400" /> : <Copy size={11} />}
          </button>
          {offersRetry && (
            <button
              className={clsx(
                'flex items-center gap-1 p-1 rounded text-text-tertiary transition-colors text-2xs',
                resendDisabledReason ? 'opacity-50 cursor-not-allowed' : 'hover:bg-surface-2 hover:text-red-400',
              )}
              {...resendProps}
              onClick={resendDisabledReason ? undefined : onRetry}
            >
              <RotateCcw size={11} />
              <span>{t('message.retry')}</span>
            </button>
          )}
          {isLastAssistant && !isError && onRegenerate && (
            <button
              className={clsx(
                'flex items-center gap-1 p-1 rounded text-text-tertiary transition-colors text-2xs',
                resendDisabledReason ? 'opacity-50 cursor-not-allowed' : 'hover:bg-surface-2 hover:text-text-secondary',
              )}
              {...resendProps}
              onClick={resendDisabledReason ? undefined : onRegenerate}
            >
              <RefreshCw size={11} />
              <span>{t('message.regenerate')}</span>
            </button>
          )}
          {resendDisabledReason && showsResend && (
            <span id={resendReasonId} className="sr-only">
              {resendDisabledReason}
            </span>
          )}
        </div>
      )}
    </div>
  )
}

export default memo(MessageBubble, (prevProps, nextProps) => {
  return (
    prevProps.message.id === nextProps.message.id &&
    prevProps.message.content === nextProps.message.content &&
    prevProps.message.finishReason === nextProps.message.finishReason &&
    prevProps.message.errorCode === nextProps.message.errorCode &&
    prevProps.message.errorShared === nextProps.message.errorShared &&
    prevProps.message.model === nextProps.message.model &&
    prevProps.message.modelLabel === nextProps.message.modelLabel &&
    prevProps.modelLabel === nextProps.modelLabel &&
    prevProps.message.servedModel === nextProps.message.servedModel &&
    prevProps.message.timestamp === nextProps.message.timestamp &&
    prevProps.message.toolCalls === nextProps.message.toolCalls &&
    prevProps.message.fixProposal === nextProps.message.fixProposal &&
    prevProps.message.fixProposals === nextProps.message.fixProposals &&
    prevProps.message.approvals === nextProps.message.approvals &&
    prevProps.message.truncated === nextProps.message.truncated &&
    prevProps.message.knowledge === nextProps.message.knowledge &&
    prevProps.onCompact === nextProps.onCompact &&
    prevProps.isStreaming === nextProps.isStreaming &&
    prevProps.isThinking === nextProps.isThinking &&
    prevProps.isLastAssistant === nextProps.isLastAssistant &&
    prevProps.onRegenerate === nextProps.onRegenerate &&
    prevProps.onRetry === nextProps.onRetry &&
    prevProps.resendDisabledReason === nextProps.resendDisabledReason &&
    prevProps.highlight === nextProps.highlight &&
    prevProps.isActiveMatch === nextProps.isActiveMatch
  )
})
