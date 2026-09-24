import {useTranslation} from 'react-i18next'
import React, {memo, useCallback, useMemo, useState} from 'react'
import clsx from 'clsx'
import {Highlight, themes} from 'prism-react-renderer'
import {Copy, Check, WrapText} from 'lucide-react'
import {useCopy} from '@/hooks/useCopy'
import {useUIStore} from '@/stores/uiStore'

interface Props {
  language?: string
  value: string
  // `plain` skips Prism entirely and renders the raw text. Used for a code
  // fence that is still streaming: tokenizing a growing block on every
  // animation frame is the single most expensive thing the chat can do, and
  // the block is re-rendered highlighted the moment the fence closes.
  plain?: boolean
}

// Pick the Prism theme by reading the active theme's `color-scheme` so we
// never need to maintain a light-theme allowlist — any theme that declares
// `color-scheme: light` automatically gets the light code theme.
//
// Cached per resolved theme at module scope: getComputedStyle forces a style
// recalc, and an answer with a dozen code blocks used to pay for one each.
let themeCacheKey: string | null = null
let themeCacheValue = themes.vsDark
function prismThemeFor(resolvedTheme: string) {
  if (typeof window === 'undefined') return themes.vsDark
  if (themeCacheKey === resolvedTheme) return themeCacheValue
  const scheme = getComputedStyle(document.documentElement).getPropertyValue('color-scheme').trim()
  themeCacheKey = resolvedTheme
  themeCacheValue = scheme === 'light' ? themes.vsLight : themes.vsDark
  return themeCacheValue
}

const PRE_STYLE: React.CSSProperties = {
  margin: 0,
  padding: '1rem',
  background: 'transparent',
  fontSize: 'var(--chat-code-size, 13px)',
  lineHeight: '1.6',
  fontFamily: 'var(--font-mono)',
}

// Long lines wrap by default, so nothing in the conversation scrolls
// sideways (in a 300-560px panel a scrolling block hid most of every line).
// The header toggle switches one block to horizontal scrolling for the
// cases where the exact column layout matters. Copy always takes the
// original text, wrapped or not.
const PRE_WRAP = 'whitespace-pre-wrap break-normal [overflow-wrap:anywhere]'
const PRE_SCROLL = 'whitespace-pre overflow-x-auto custom-scrollbar'

// prism-react-renderer names token classes after the grammar's token types
// ("token table", "token block", …). Some of those are also Tailwind
// utilities — `table` is `display: table`, `block` is `display: block` — so a
// markdown table inside a code fence rendered one cell per line. The colours
// come from the theme's inline styles, so the classes are free to move out
// of Tailwind's namespace.
function tokenClassName(types: string[]): string {
  return types.map(type => `tok-${type}`).join(' ')
}

const headerButton = 'flex items-center justify-center h-7 rounded-md transition-colors duration-fast'

function CodeBlock({language, value, plain}: Props) {
  const {t} = useTranslation('chat')
  const {copied, copy} = useCopy()
  const [wrap, setWrap] = useState(true)
  const resolvedTheme = useUIStore(s => s.resolvedTheme)
  // resolvedTheme is the TRIGGER to re-read `color-scheme` off the DOM; the
  // lookup body intentionally reads computed style, not the theme value.
  const prismTheme = useMemo(() => prismThemeFor(resolvedTheme), [resolvedTheme])

  const handleCopy = useCallback(() => {
    copy(value)
  }, [value, copy])
  const toggleWrap = useCallback(() => setWrap(w => !w), [])

  const code = useMemo(() => value.replace(/\n$/, ''), [value])
  const preClass = wrap ? PRE_WRAP : PRE_SCROLL
  const wrapLabel = t('code.wrapLines')

  return (
    <div className="relative group min-w-0 max-w-full rounded-xl overflow-hidden border border-border-default my-3 bg-surface-0">
      <div className="flex items-center justify-between gap-2 pl-4 pr-2 py-1.5 bg-surface-3/80 backdrop-blur-sm border-b border-border-subtle">
        <span className="min-w-0 truncate text-xs font-mono text-text-tertiary select-none">{language || 'text'}</span>
        <div className="flex items-center gap-0.5 shrink-0">
          <button
            type="button"
            onClick={toggleWrap}
            aria-pressed={wrap}
            aria-label={wrapLabel}
            title={wrapLabel}
            className={clsx(
              headerButton,
              'w-7',
              wrap
                ? 'text-brand-400 bg-brand-500/10 hover:bg-brand-500/15'
                : 'text-text-tertiary hover:text-text-primary hover:bg-surface-4',
            )}
          >
            <WrapText size={14} aria-hidden="true" />
          </button>
          <button
            type="button"
            onClick={handleCopy}
            className={clsx(headerButton, 'gap-1.5 px-2 text-text-tertiary hover:text-text-primary hover:bg-surface-4')}
            title={t('code.copyTitle')}
          >
            {copied ? (
              <>
                <Check size={14} className="text-semantic-success" aria-hidden="true" />
                <span className="text-xs font-medium text-semantic-success">{t('code.copied')}</span>
              </>
            ) : (
              <>
                <Copy size={14} aria-hidden="true" />
                <span className="text-xs font-medium">{t('code.copy')}</span>
              </>
            )}
          </button>
        </div>
      </div>

      {plain ? (
        <pre style={PRE_STYLE} className={clsx('text-text-secondary', preClass)}>
          {code}
        </pre>
      ) : (
        <Highlight theme={prismTheme} code={code} language={language || 'text'}>
          {({className, style, tokens, getLineProps, getTokenProps}) => (
            <pre className={clsx(className, preClass)} style={{...style, ...PRE_STYLE}}>
              {tokens.map((line, i) => {
                const lineProps = getLineProps({line})
                return (
                  <div key={i} {...lineProps}>
                    {line.map((token, key) => {
                      const {className: _prismClass, ...tokenProps} = getTokenProps({token})
                      return <span key={key} {...tokenProps} className={tokenClassName(token.types)} />
                    })}
                  </div>
                )
              })}
            </pre>
          )}
        </Highlight>
      )}
    </div>
  )
}

// Prism tokenizes the whole block on every render. Memoizing on the code text
// keeps a settled block off the streaming re-render path entirely.
export default memo(CodeBlock)
