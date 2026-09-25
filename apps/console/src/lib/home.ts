/** What the home page derives from the API's answers (routes/home.tsx). */
import type { AuditEvent } from './api'
import type { MessageKey } from './messages'

/** Audit actions that start a deployment run, and how the home page names them. */
export const DEPLOY_ACTIONS: Record<string, MessageKey> = {
  startDeployment: 'home.deploy.startDeployment',
  rollbackApp: 'home.deploy.rollbackApp',
  emergencyRollback: 'home.deploy.emergencyRollback',
  promoteApp: 'home.deploy.promoteApp',
}

/** The deployments among audit events: accepted, newest first, at most `limit`. */
export function recentDeployments(events: readonly AuditEvent[], limit: number): AuditEvent[] {
  return events
    .filter(
      (e) => Object.hasOwn(DEPLOY_ACTIONS, e.action) && (e.outcome === 'accepted' || e.outcome === 'success'),
    )
    .sort((a, b) => b.at - a.at)
    .slice(0, limit)
}

export interface Subsystem {
  name: string
  state: string
  lastError?: string
}

/** The health details' subsystems (`{name: {state, last_error}}`), sorted by name. */
export function subsystemsOf(raw: unknown): Subsystem[] {
  if (!raw || typeof raw !== 'object') return []
  return Object.entries(raw as Record<string, unknown>)
    .map(([name, value]) => {
      const v = (value ?? {}) as { state?: unknown; last_error?: unknown }
      return {
        name,
        state: typeof v.state === 'string' ? v.state : 'unknown',
        lastError: typeof v.last_error === 'string' && v.last_error ? v.last_error : undefined,
      }
    })
    .sort((a, b) => a.name.localeCompare(b.name))
}
