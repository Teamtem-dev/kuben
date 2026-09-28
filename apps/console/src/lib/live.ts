import { type QueryClient, useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'
import type { Build } from './delivery'

/** Query-key roots to refresh for a stream delta `kind` (e.g. `pod_upsert`). */
export function keysFor(kind: string): readonly string[] {
  if (kind.startsWith('pod_')) return ['app']
  if (kind.startsWith('project_')) return ['projects']
  if (kind.startsWith('environment_')) return ['environments', 'projects']
  if (kind.startsWith('app_') || kind === 'exposure_changed') return ['apps', 'app']
  // A build started, moved on or settled: the builds tab and its detail
  // (while a stream has no build deltas, the tab polls running builds).
  if (kind === 'build' || kind.startsWith('build_')) return ['app']
  return []
}

const EVERYTHING = ['projects', 'environments', 'apps', 'app'] as const

function parse(data: string): Record<string, unknown> | null {
  try {
    const value: unknown = JSON.parse(data)
    return typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : null
  } catch {
    return null
  }
}

const kindOf = (value: Record<string, unknown> | null) => (typeof value?.kind === 'string' ? value.kind : '')

/** A `build` delta: the build as it now stands, and the app it belongs to. */
export interface BuildDelta {
  project: string
  environment: string
  app: string
  build: Build
}

/** The build a `build` delta carries, or `null` when the delta is not one (or not whole). */
export function buildDelta(value: Record<string, unknown> | null): BuildDelta | null {
  if (value?.kind !== 'build') return null
  const { project, environment, name, build } = value
  if (typeof project !== 'string' || typeof environment !== 'string' || typeof name !== 'string') return null
  if (typeof build !== 'object' || build === null) return null
  const b = build as Partial<Build>
  if (typeof b.id !== 'string' || typeof b.phase !== 'string' || typeof b.createdAt !== 'number') return null
  return { project, environment, app: name, build: build as Build }
}

/** The builds list with `build` in it: replaced where it was, else added in front (newest first). */
export function upsertBuild(list: readonly Build[], build: Build): Build[] {
  const at = list.findIndex((b) => b.id === build.id)
  if (at >= 0) return list.map((b, i) => (i === at ? build : b))
  return [build, ...list].sort((a, z) => z.createdAt - a.createdAt)
}

/** Apply a build delta to the cached builds list and the build's detail. */
function applyBuild(queryClient: QueryClient, delta: BuildDelta) {
  const builds = ['app', delta.project, delta.environment, delta.app, 'builds']
  queryClient.setQueryData<Build[]>(builds, (list) => (list ? upsertBuild(list, delta.build) : list))
  queryClient.setQueryData<Build>([...builds, delta.build.id], (current) => (current ? delta.build : current))
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
    source.addEventListener('delta', (event: MessageEvent<string>) => {
      const value = parse(event.data)
      // Builds are not in the snapshot: a delta carries the whole build, applied as it is.
      const build = buildDelta(value)
      if (build) applyBuild(queryClient, build)
      else schedule(keysFor(kindOf(value)))
    })
    source.addEventListener('resync', () => schedule(EVERYTHING))
    return () => {
      source.close()
      if (timer) clearTimeout(timer)
    }
  }, [queryClient])
}
