import {useState, useEffect} from 'react'
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
}

function clamp(n: number, min?: number, max?: number): number {
    if (min !== undefined && n < min) return min
    if (max !== undefined && n > max) return max
    return n
}

/**
 * Numeric input that commits a validated value on blur/Enter (not per-keystroke,
 * so a min>1 field doesn't fight you mid-type). Invalid input falls back to
 * `fallback`; out-of-range input is clamped to [min, max] — neither is silently
 * persisted. Wraps the shared Input.
 */
export default function NumberField({value, onCommit, min, max, step, fallback, integer = true, className}: Props) {
    const [draft, setDraft] = useState(String(value))

    // Reflect external value changes (store reset, switching provider, etc.).
    useEffect(() => { setDraft(String(value)) }, [value])

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
            onBlur={commit}
            onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); (e.target as HTMLInputElement).blur() } }}
            className={className}
        />
    )
}
