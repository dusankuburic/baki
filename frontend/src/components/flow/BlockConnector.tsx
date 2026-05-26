type BlockConnectorProps = {
    isActive?: boolean
}

export default function BlockConnector({isActive = false}: BlockConnectorProps) {
    const stroke = isActive ? 'var(--brand-500)' : 'var(--border-strong)'
    const className = isActive ? 'animate-dash' : ''

    return (
        <svg width={16} height={14} className={`mx-auto ${className}`}>
            <line
                x1={8} y1={0} x2={8} y2={10}
                stroke={stroke}
                strokeWidth={2}
                strokeDasharray={isActive ? 4 : undefined}
            />
            <path
                d="M 4 8 L 8 12 L 12 8"
                stroke={stroke}
                fill="none"
                strokeWidth={2}
                strokeLinecap="round"
            />
        </svg>
    )
}
