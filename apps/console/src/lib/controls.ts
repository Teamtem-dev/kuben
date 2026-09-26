/** Helpers of the operational controls' views (routes/ops/controls.tsx). */

export type WindowState = 'active' | 'scheduled' | 'lifted' | 'ended'

/** Where a freeze or silence stands at `now` (Unix ms). */
export function windowState(
  w: { active: boolean; liftedAt?: string | null; startsAt: string; endsAt: string },
  now: number,
): WindowState {
  if (w.liftedAt) return 'lifted'
  if (w.active) return 'active'
  return Date.parse(w.startsAt) > now ? 'scheduled' : 'ended'
}

const pad = (n: number) => String(n).padStart(2, '0')

/** A time as an `<input type="datetime-local">` value, in the browser's zone. */
export function localInput(ms: number): string {
  const d = new Date(ms)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** A datetime-local value (the browser's zone) as RFC 3339 in UTC, or null when empty or invalid. */
export function rfc3339(local: string): string | null {
  if (!local) return null
  const ms = new Date(local).getTime()
  return Number.isNaN(ms) ? null : new Date(ms).toISOString()
}

/** Lines of a textarea as a list: trimmed, blanks dropped. */
export function lines(text: string): string[] {
  return text
    .split(/[\n,]/)
    .map((l) => l.trim())
    .filter(Boolean)
}
