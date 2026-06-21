import {useCallback, useMemo} from 'react'
import {Highlight, themes} from 'prism-react-renderer'
import {Copy, Check} from 'lucide-react'
import {useCopy} from '@/hooks/useCopy'
import {useUIStore} from '@/stores/uiStore'

interface Props {
  language?: string
  value: string
}

export default function CodeBlock({language, value}: Props) {
  const {copied, copy} = useCopy()
  const resolvedTheme = useUIStore(s => s.resolvedTheme)
  // Pick the Prism theme by reading the active theme's `color-scheme` so we
  // never need to maintain a light-theme allowlist — any theme that declares
  // `color-scheme: light` automatically gets the light code theme.
  const prismTheme = useMemo(() => {
    if (typeof window === 'undefined') return themes.vsDark
    const scheme = getComputedStyle(document.documentElement).getPropertyValue('color-scheme').trim()
    return scheme === 'light' ? themes.vsLight : themes.vsDark
  }, [resolvedTheme])

  const handleCopy = useCallback(() => {
    copy(value)
  }, [value, copy])

  const code = useMemo(() => value.replace(/\n$/, ''), [value])

  return (
    <div className="relative group rounded-xl overflow-hidden border border-border-default my-3 bg-surface-0">
      <div className="flex items-center justify-between px-4 py-2 bg-surface-3/80 backdrop-blur-sm border-b border-border-subtle">
        <span className="text-xs font-mono text-text-tertiary select-none">
          {language || 'text'}
        </span>
        <button
          onClick={handleCopy}
          className="flex items-center gap-1.5 px-2 py-1 rounded-md text-text-tertiary hover:text-text-primary hover:bg-surface-4 transition-all duration-200"
          title="Copy code"
        >
          {copied ? (
            <>
              <Check size={14} className="text-green-400" />
              <span className="text-xs font-medium text-green-400">Copied</span>
            </>
          ) : (
            <>
              <Copy size={14} />
              <span className="text-xs font-medium">Copy</span>
            </>
          )}
        </button>
      </div>

      <Highlight
        theme={prismTheme}
        code={code}
        language={language || 'text'}
      >
        {({className, style, tokens, getLineProps, getTokenProps}) => (
          <pre
            className={className}
            style={{
              ...style,
              margin: 0,
              padding: '1rem',
              background: 'transparent',
              fontSize: '13px',
              lineHeight: '1.6',
              fontFamily: '"JetBrains Mono", "Fira Code", monospace',
              overflowX: 'auto',
            }}
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
    </div>
  )
}
