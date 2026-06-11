import {useState, useCallback} from 'react'
import {PrismAsyncLight as SyntaxHighlighter} from 'react-syntax-highlighter'
import {vscDarkPlus} from 'react-syntax-highlighter/dist/esm/styles/prism'
import {Copy, Check} from 'lucide-react'

interface Props {
  language?: string
  value: string
}

export default function CodeBlock({language, value}: Props) {
  const [copied, setCopied] = useState(false)

  const handleCopy = useCallback(() => {
    if (typeof window === 'undefined') return
    navigator.clipboard.writeText(value).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    })
  }, [value])

  return (
    <div className="relative group rounded-xl overflow-hidden border border-border-default my-3 bg-[#1e1e1e]">
      {/* Code Header Bar */}
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

      {/* Actual Code Area */}
      <div className="overflow-x-auto scrollbar-thin scrollbar-thumb-white/10 scrollbar-track-transparent">
        <SyntaxHighlighter
          language={language || 'text'}
          style={vscDarkPlus}
          customStyle={{
            margin: 0,
            padding: '1rem',
            background: 'transparent',
            fontSize: '13px',
            lineHeight: '1.6',
          }}
          codeTagProps={{
            style: {
              fontFamily: '"JetBrains Mono", "Fira Code", monospace',
            }
          }}
        >
          {value}
        </SyntaxHighlighter>
      </div>
    </div>
  )
}
