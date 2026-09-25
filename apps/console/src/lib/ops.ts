/** Formatting helpers of the operational views (M5.6). */

/** A span of seconds in its two largest units, in `locale`: `2 h 5 min`. */
export function duration(seconds: number, locale: string): string {
  const units: [Intl.NumberFormatOptions['unit'], number][] = [
    ['day', 86_400],
    ['hour', 3_600],
    ['minute', 60],
  ]
  let rest = Math.max(0, Math.floor(seconds))
  const parts: string[] = []
  for (const [unit, size] of units) {
    const n = Math.floor(rest / size)
    if (n > 0 && parts.length < 2) {
      parts.push(new Intl.NumberFormat(locale, { style: 'unit', unit, unitDisplay: 'short' }).format(n))
      rest -= n * size
    }
  }
  if (parts.length === 0) {
    return new Intl.NumberFormat(locale, { style: 'unit', unit: 'minute', unitDisplay: 'short' }).format(0)
  }
  return parts.join(' ')
}

/** Bytes in binary units: `64 MiB`. */
export function bytes(n: number, locale: string): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let value = n
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i += 1
  }
  const digits = value >= 10 || i === 0 ? 0 : 1
  return `${new Intl.NumberFormat(locale, { maximumFractionDigits: digits }).format(value)} ${units[i]}`
}

/** CPU in millicores or cores: `250m`, `1.5 cores`. */
export function cpu(millicores: number, locale: string, coresLabel: string): string {
  if (millicores < 1000) return `${new Intl.NumberFormat(locale).format(millicores)}m`
  return `${new Intl.NumberFormat(locale, { maximumFractionDigits: 2 }).format(millicores / 1000)} ${coresLabel}`
}

/** A date in `locale`, or the text itself when it is not one. */
export function when(value: string | null | undefined, locale: string): string {
  if (!value) return ''
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString(locale)
}

/** The points of an SVG polyline drawing `values` in a `width`×`height` box. */
export function sparkline(values: readonly number[], width: number, height: number): string {
  if (values.length === 0) return ''
  const max = Math.max(...values, 1)
  const step = values.length > 1 ? width / (values.length - 1) : 0
  return values
    .map((v, i) => {
      const x = values.length > 1 ? i * step : width / 2
      const y = height - (v / max) * height
      return `${Math.round(x * 10) / 10},${Math.round(y * 10) / 10}`
    })
    .join(' ')
}

/** Where an incident stands. */
export function incidentState(i: {
  resolvedAt?: string | null
  acknowledgedAt?: string | null
}): 'resolved' | 'acknowledged' | 'open' {
  if (i.resolvedAt) return 'resolved'
  if (i.acknowledgedAt) return 'acknowledged'
  return 'open'
}

/** The colour family of a state or severity (the kit's `ToneBadge` draws it). */
export type Tone = 'success' | 'warning' | 'danger' | 'neutral'

/** The tone of each state or severity the operational views show. */
export const stateTones: Record<string, Tone> = {
  critical: 'danger',
  warning: 'warning',
  info: 'neutral',
  open: 'danger',
  acknowledged: 'warning',
  resolved: 'success',
  verified: 'success',
  pending: 'warning',
  revoked: 'neutral',
  delivered: 'success',
  failed: 'danger',
  active: 'success',
  closed: 'neutral',
}

/** The webhook events a person can pick. */
export const WEBHOOK_EVENTS = [
  'deployment.started',
  'deployment.succeeded',
  'deployment.failed',
  'deployment.cancelled',
  'build.started',
  'build.succeeded',
  'build.failed',
  'build.cancelled',
  'incident.opened',
  'incident.resolved',
] as const
