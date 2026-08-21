import { useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'

/** Query-key roots to refresh for a stream delta `kind` (e.g. `pod_upsert`). */
export function keysFor(kind: string): readonly string[] {
  if (kind.startsWith('pod_')) return ['app']
  if (kind.startsWith('project_')) return ['projects']
  if (kind.startsWith('environment_')) return ['environments', 'projects']
  if (kind.startsWith('app_')) return ['apps', 'app']
  return []
}

const EVERYTHING = ['projects', 'environments', 'apps', 'app'] as const

function kindOf(data: string): string {
  try {
    const value: unknown = JSON.parse(data)
    return typeof value === 'object' && value !== null && 'kind' in value && typeof value.kind === 'string'
      ? value.kind
      : ''
  } catch {
    return ''
  }
}

/**
 * One SSE connection per tab (ADR-014). Deltas invalidate the affected
 * queries, batched so a rollout touching 50 pods causes one refetch.
 */
export function useLiveUpdates() {
  const queryClient = useQueryClient()
  useEffect(() => {
    const pending = new Set<string>()
    let timer: ReturnType<typeof setTimeout> | undefined
    const flush = () => {
      timer = undefined
      for (const key of pending) void queryClient.invalidateQueries({ queryKey: [key] })
      pending.clear()
    }
    const schedule = (keys: readonly string[]) => {
      for (const key of keys) pending.add(key)
      if (pending.size > 0) timer ??= setTimeout(flush, 300)
    }
    const source = new EventSource('/api/v1/stream')
    source.addEventListener('delta', (event: MessageEvent<string>) => schedule(keysFor(kindOf(event.data))))
