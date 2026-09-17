import type { ReactNode } from 'react'
import { sparkline, tones } from '../lib/ops'

/** A state or severity as a small pill. */
export function Pill({ tone, children }: { tone: string; children: ReactNode }) {
  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-full border px-2 py-0.5 font-medium text-xs ${tones[tone] ?? tones.info}`}
    >
      {children}
    </span>
  )
}

/** A line chart of `values`, labelled for screen readers. */
export function Sparkline({ values, label }: { values: readonly number[]; label: string }) {
  const points = sparkline(values, 240, 48)
  return (
    <svg
      viewBox="0 0 240 48"
      className="h-12 w-full"
      role="img"
      aria-label={label}
      preserveAspectRatio="none"
    >
      <polyline
        points={points}
        fill="none"
        className="stroke-accent"
        strokeWidth="2"
        vectorEffect="non-scaling-stroke"
        strokeLinejoin="round"
      />
    </svg>
  )
}

/** A value that can be copied: the text and a copy button. */
export function Copyable({ value, label }: { value: string; label: string }) {
  return (
    <span className="flex min-w-0 items-center gap-2">
      <code dir="ltr" className="min-w-0 break-all rounded bg-inset px-1.5 py-0.5 text-xs">
        {value}
      </code>
      <button
        type="button"
        className="shrink-0 rounded-md border border-line px-2 py-0.5 text-xs hover:bg-hover"
        onClick={() => void navigator.clipboard?.writeText(value)}
      >
        {label}
      </button>
    </span>
  )
}
