type VariableChipsProps = {
    variables: string[]
    onVariableClick?: (variable: string) => void
}

export default function VariableChips({variables, onVariableClick}: VariableChipsProps) {
    if (variables.length === 0) return null

    return (
        <div className="flex flex-wrap gap-1.5">
            {variables.map(v => (
                <button
                    key={v}
                    onClick={() => onVariableClick?.(v)}
                    className="bg-block-variable-bg text-block-variable text-xs px-2 py-1 rounded-md font-mono border border-block-variable/10 hover:bg-block-variable-bg/20 hover:border-block-variable/20 transition-all duration-fast"
                >
                    {v}
                </button>
            ))}
        </div>
    )
}
