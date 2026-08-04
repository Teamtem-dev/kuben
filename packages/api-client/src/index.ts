import createClient from 'openapi-fetch'
import type { paths } from './schema'

export type { components, paths } from './schema'

/** Header the API requires on cookie-authenticated mutations (CSRF guard). */
export const CLIENT_HEADER = 'x-kuben-client'

/**
 * Typed client for the Kuben REST API. Same-origin only: the session is an
 * `HttpOnly` cookie, so JavaScript never sees a token.
 */
export const api = createClient<paths>({
  credentials: 'same-origin',
  headers: { [CLIENT_HEADER]: 'web' },
})
