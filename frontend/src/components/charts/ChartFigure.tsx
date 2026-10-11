import type {ReactElement} from 'react'
import clsx from 'clsx'
import {ResponsiveContainer} from 'recharts'

export function ChartFigure({
  height,
  children,
  className,
}: {
  height: number
  children: ReactElement
  className?: string
}) {
  return (
    <div className={clsx('chart-figure w-full', className)} style={{height}}>
      <ResponsiveContainer width="100%" height="100%">
        {children}
      </ResponsiveContainer>
    </div>
  )
}
