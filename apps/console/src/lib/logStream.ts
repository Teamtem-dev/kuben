import type { LogEnd, LogLine } from './api'

/** At most this many lines stay on the page; the oldest go first. */
export const MAX_LINES = 2000

export interface ShownLine {
  id: number
  pod: string
  time?: string | null
  text: string
  /** A note from the server (a pod's log ended), not a line of the app. */
  note?: boolean
}

/** Lines in arrival order, bounded. */
export function appendLines(
  lines: readonly ShownLine[],
  more: readonly ShownLine[],
  max = MAX_LINES,
): ShownLine[] {
  const all = lines.concat(more)
  return all.length > max ? all.slice(all.length - max) : all
}

function parse<T>(data: string): T | undefined {
  try {
    return JSON.parse(data) as T
  } catch {
    return undefined
  }
}

export function lineFrom(data: string, id: number): ShownLine | undefined {
  const line = parse<LogLine>(data)
  return line && typeof line.line === 'string'
    ? { id, pod: line.pod, time: line.time, text: line.line }
    : undefined
}

/** The note an `end` event leaves, and whether the whole stream ended. */
export function endFrom(data: string, id: number): { note: ShownLine; final: boolean } | undefined {
  const end = parse<LogEnd>(data)
  if (!end) return undefined
  const final = !end.pod
  return {
    final,
    note: { id, pod: end.pod ?? '', text: end.error ?? (final ? 'stream ended' : 'log ended'), note: true },
  }
}

/** `2026-09-16T10:00:00.123456789Z message` split into time and text. */
export function splitTimestamp(raw: string): { time?: string; text: string } {
  const space = raw.indexOf(' ')
  const head = space > 0 ? raw.slice(0, space) : ''
  return /^\d{4}-\d{2}-\d{2}T/.test(head) ? { time: head, text: raw.slice(space + 1) } : { text: raw }
}
