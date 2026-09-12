import type { components } from '@kuben/api-client'

/** RFC 9457 problem body returned by every failing API call. */
export type Problem = components['schemas']['Problem']

export function isProblem(value: unknown): value is Problem {
  return (
    typeof value === 'object' && value !== null && 'status' in value && 'code' in value && 'title' in value
  )
}

/** Thrown from query/mutation functions so TanStack Query receives a real `Error`. */
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(problem: Problem) {
    super(problem.detail ?? problem.title)
    this.name = 'ApiError'
    this.status = problem.status
    this.code = problem.code
  }
}

export function toApiError(body: unknown, status: number): ApiError {
  return new ApiError(isProblem(body) ? body : { code: 'unknown', title: 'Request failed', status })
}

/** Human-readable message for anything a request can throw. */
export function problemMessage(error: unknown, fallback = 'Something went wrong.'): string {
  if (isProblem(error)) return error.detail ?? error.title
  if (error instanceof Error && error.message) return error.message
  return fallback
}
