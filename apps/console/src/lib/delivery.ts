/**
 * Pure helpers of the app page's delivery views: deployment approvals (the
 * decision a person sends back, with the plan hash they were shown) and the
 * builds of a Git-sourced app.
 */
import type { components } from '@kuben/api-client'
import type { Tone } from './ops'

type Schemas = components['schemas']
export type Approval = Schemas['ApprovalDto']
export type Decision = Schemas['DecisionDto']
export type DecideRequest = Schemas['DecideRequest']
export type DeploymentRun = Schemas['DeploymentDto']
export type Build = Schemas['BuildDto']
export type AppScans = Schemas['AppScansDto']

/** The phase of a run that waits for approvals before it is delivered. */
export const AWAITING_APPROVAL = 'awaitingApproval'

export const isAwaitingApproval = (phase: string) => phase === AWAITING_APPROVAL

/** The longest comment a decision may carry (the API's limit). */
export const COMMENT_MAX = 1024

/**
 * The body of an approve or reject: the plan hash the person was shown, and
 * their comment when they wrote one. `null` when there is no plan to confirm.
 */
export function decideBody(planHash: string | null | undefined, comment: string): DecideRequest | null {
  if (!planHash) return null
  const text = comment.trim()
  return text ? { planHash, comment: text.slice(0, COMMENT_MAX) } : { planHash }
}

/** How many more distinct approvals the run needs. */
export const approvalsLeft = (a: Pick<Approval, 'required' | 'approved'>) =>
  Math.max(0, a.required - a.approved)

/** `12.4s`, `3m 05s`, `1h 02m`. */
export function elapsed(ms: number): string {
  const seconds = Math.max(0, ms) / 1000
  if (seconds < 10) return `${seconds.toFixed(1)}s`
  if (seconds < 60) return `${Math.floor(seconds)}s`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ${String(Math.floor(seconds % 60)).padStart(2, '0')}s`
  return `${Math.floor(minutes / 60)}h ${String(minutes % 60).padStart(2, '0')}m`
}

/** Phases a build does not leave. */
const BUILD_FINAL = new Set(['succeeded', 'failed', 'cancelled'])
export const isFinalBuild = (phase: string) => BUILD_FINAL.has(phase)

/** Whether a stop can still be asked for: the build is under way and nobody asked yet. */
export const canCancelBuild = (b: Pick<Build, 'phase' | 'cancelRequested'>) =>
  !isFinalBuild(b.phase) && !b.cancelRequested && b.phase !== 'cancelRequested' && b.phase !== 'cancelling'

/** The colour of a build's phase. */
export function buildTone(phase: string): Tone {
  if (phase === 'succeeded') return 'success'
  if (phase === 'failed') return 'danger'
  if (phase === 'blocked' || phase === 'cancelRequested' || phase === 'cancelling') return 'warning'
  return 'neutral'
}

/** How long the build ran (or has been running at `now`); `null` before it started. */
export function buildElapsed(b: Pick<Build, 'startedAt' | 'finishedAt'>, now: number): number | null {
  if (b.startedAt == null) return null
  return Math.max(0, (b.finishedAt ?? now) - b.startedAt)
}

/** A commit as people read it: its first 7 characters. */
export const shortCommit = (commit: string) => (/^[0-9a-f]{8,}$/i.test(commit) ? commit.slice(0, 7) : commit)

/** A build or run id as people read it: the first 8 characters of a UUID. */
export const shortId = (id: string) => (/^[0-9a-f]{8}-[0-9a-f-]+$/i.test(id) ? id.slice(0, 8) : id)

/** The digest of `repository@digest`, or `null` for a reference without one. */
export function imageDigest(image: string | null | undefined): string | null {
  const at = image?.lastIndexOf('@') ?? -1
  return image && at >= 0 && at < image.length - 1 ? image.slice(at + 1) : null
}

/** The address of an image's SBOM (CycloneDX JSON; the browser undoes the gzip). */
export function sbomUrl(project: string, environment: string, app: string, digest: string): string {
  const path = [project, environment, app, digest].map(encodeURIComponent)
  return `/api/v1/projects/${path[0]}/environments/${path[1]}/apps/${path[2]}/sbom/${path[3]}`
}
