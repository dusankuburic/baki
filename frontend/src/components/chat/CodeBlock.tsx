import {useTranslation} from 'react-i18next'
import React, {memo, useCallback, useMemo} from 'react'
import {Highlight, themes} from 'prism-react-renderer'
import {Copy, Check} from 'lucide-react'
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
  fontSize: '13px',
  lineHeight: '1.6',
  fontFamily: 'var(--font-mono)',
  overflowX: 'auto',
}

function CodeBlock({language, value, plain}: Props) {
  const {t} = useTranslation('chat')
  const {copied, copy} = useCopy()
  const resolvedTheme = useUIStore(s => s.resolvedTheme)
  // resolvedTheme is the TRIGGER to re-read `color-scheme` off the DOM; the
  // lookup body intentionally reads computed style, not the theme value.
  const prismTheme = useMemo(() => prismThemeFor(resolvedTheme), [resolvedTheme])

  const handleCopy = useCallback(() => {
    copy(value)
  }, [value, copy])

  const code = useMemo(() => value.replace(/\n$/, ''), [value])

  return (
    <div className="relative group rounded-xl overflow-hidden border border-border-default my-3 bg-surface-0">
      <div className="flex items-center justify-between px-4 py-2 bg-surface-3/80 backdrop-blur-sm border-b border-border-subtle">
        <span className="text-xs font-mono text-text-tertiary select-none">{language || 'text'}</span>
        <button
          onClick={handleCopy}
          className="flex items-center gap-1.5 px-2 py-1 rounded-md text-text-tertiary hover:text-text-primary hover:bg-surface-4 transition-all duration-200"
          title={t('code.copyTitle')}
        >
          {copied ? (
            <>
              <Check size={14} className="text-semantic-success" />
              <span className="text-xs font-medium text-semantic-success">{t('code.copied')}</span>
            </>
          ) : (
            <>
              <Copy size={14} />
              <span className="text-xs font-medium">{t('code.copy')}</span>
            </>
          )}
        </button>
      </div>

      {plain ? (
        <pre style={PRE_STYLE} className="text-text-secondary">
          {code}
        </pre>
      ) : (
        <Highlight theme={prismTheme} code={code} language={language || 'text'}>
          {({className, style, tokens, getLineProps, getTokenProps}) => (
            <pre
              className={className}
              style={{...style, ...PRE_STYLE}}
            >
              {tokens.map((line, i) => {
                const lineProps = getLineProps({line})
                return (
                  <div key={i} {...lineProps}>
                    {line.map((token, key) => {
                      const tokenProps = getTokenProps({token})
                      return <span key={key} {...tokenProps} />
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
