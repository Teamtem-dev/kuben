import { useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'

/** Query-key roots to refresh for a stream delta `kind` (e.g. `pod_upsert`). */
export function keysFor(kind: string): readonly string[] {
  if (kind.startsWith('pod_')) return ['app']
