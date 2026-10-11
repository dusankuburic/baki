import {useState} from 'react'
import Input from './Input'

type Props = {
  value: number
  onCommit: (n: number) => void
  min?: number
  max?: number
  step?: number | string
  fallback: number
  integer?: boolean
  className?: string
  error?: string
  hint?: string
  onBlur?: () => void
}

function clamp(n: number, min?: number, max?: number): number {
  if (min !== undefined && n < min) return min
  if (max !== undefined && n > max) return max
  return n
}

export default function NumberField({
  value,
  onCommit,
  min,
  max,
  step,
  fallback,
  integer = true,
  className,
  error,
  hint,
  onBlur,
}: Props) {
  const [draft, setDraft] = useState(String(value))

  const [lastValue, setLastValue] = useState(value)
  if (value !== lastValue) {
    setLastValue(value)
    setDraft(String(value))
  }

  const commit = () => {
    const parsed = integer ? parseInt(draft, 10) : parseFloat(draft)
    const next = Number.isNaN(parsed) ? fallback : clamp(parsed, min, max)
    if (next !== value) onCommit(next)
    setDraft(String(next))
  }

  return (
    <Input
      type="number"
      min={min}
      max={max}
      step={step}
      value={draft}
      onChange={e => setDraft(e.target.value)}
      onBlur={() => {
        commit()
        onBlur?.()
      }}
      onKeyDown={e => {
        if (e.key === 'Enter') {
          e.preventDefault()
          ;(e.target as HTMLInputElement).blur()
        }
      }}
      className={className}
      error={error}
      hint={hint}
    />
  )
}
