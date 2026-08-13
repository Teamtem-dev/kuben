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
